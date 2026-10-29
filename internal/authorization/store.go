package authorization

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrPolicyUnavailable means the captured policy cannot authorize a new decision.
var ErrPolicyUnavailable = errors.New("authorization policy is unavailable")

// Store owns a target's watch-driven policy and namespace generations.
// Read failures preserve valid data. Invalidation retires the affected collection;
// shutdown retires both.
type Store struct {
	target    client.WithWatch
	namespace string
	logger    *zap.Logger
	done      chan struct{}

	mu               sync.RWMutex
	cancel           context.CancelFunc
	stopping         <-chan struct{}
	closed           bool
	policy           collectionState
	namespaceState   collectionState
	generation       *policyGeneration
	namespaces       *namespaceGeneration
	listVersion      string
	namespaceVersion string
}

// collectionState keeps retries and publication barriers independent per collection.
// Its metadata is protected by Store.mu; loadMu serializes its authoritative reads.
type collectionState struct {
	source      string
	loadMu      sync.Mutex
	queue       workqueue.TypedInterface[struct{}]
	observing   bool
	observation uint64
	epoch       uint64
}

type namespaceGeneration struct {
	byName     map[string]*corev1.Namespace
	names      []string
	identities map[string]recordIdentity
}

type policyGeneration struct {
	evaluators *policyEvaluators
	records    *policyRecords
	identities map[string]recordIdentity
}

type recordIdentity struct {
	uid     types.UID
	version string
}

// NewStore prepares a policy store without starting background work.
//
// The target must be a direct reusable service-account client, not an informer-backed
// client. The logger should identify the target cluster.
func NewStore(target client.WithWatch, cfg config.ClusterAuthorizationConfig, logger *zap.Logger) (*Store, error) {
	if target == nil || cfg.PolicyNamespace == "" || logger == nil {
		return nil, errors.New("policy store requires a client, policy namespace and logger")
	}
	return &Store{
		target: target, namespace: cfg.PolicyNamespace, logger: logger,
		policy:         collectionState{source: "policy", queue: workqueue.NewTyped[struct{}]()},
		namespaceState: collectionState{source: "namespaces", queue: workqueue.NewTyped[struct{}]()},
		done:           make(chan struct{}),
	}, nil
}

// Start begins observation and reload workers under the server lifecycle context.
// It returns immediately; Snapshot fails closed until a complete policy is ready.
// A store can be started only once.
func (s *Store) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.cancel != nil {
		return errors.New("policy store is already started or stopped")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.stopping = ctx.Done()
	go s.run(ctx)
	return nil
}

// Stop retires all snapshots, cancels outstanding work and waits for workers.
// Repeated calls are safe, including before Start.
func (s *Store) Stop() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.generation = nil
		s.namespaces = nil
		s.policy.epoch++
		if s.cancel == nil {
			close(s.done)
		}
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.policy.queue.ShutDown()
	s.namespaceState.queue.ShutDown()
	<-s.done
}

// Invalidate immediately retires captured policy and schedules an authoritative
// reload. Work begun before invalidation cannot restore the retired generation.
func (s *Store) Invalidate() {
	s.mu.Lock()
	s.policy.epoch++
	s.generation = nil
	s.mu.Unlock()
	s.policy.queue.Add(struct{}{})
}

// InvalidateNamespaces retires namespace metadata and schedules an authoritative
// reload without invalidating policy-only decisions or recompiling policy.
func (s *Store) InvalidateNamespaces() {
	s.mu.Lock()
	s.namespaceState.epoch++
	s.namespaces = nil
	s.mu.Unlock()
	s.namespaceState.queue.Add(struct{}{})
}

// Snapshot captures policy, available namespace metadata and verified subjects
// without reading Kubernetes or compiling evaluators.
//
// A changed generation or invalidation retires dependent checks. Policy-only
// checks need no namespace data.
func (s *Store) Snapshot(credential auth.ClusterCredential) (*Snapshot, error) {
	subjects, err := verifiedSubjects(credential)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.usable(s.generation) {
		return nil, ErrPolicyUnavailable
	}
	return &Snapshot{
		store: s, generation: s.generation,
		namespaces: s.namespaces, namespaceVersion: s.namespaceVersion,
		subjects:    subjects,
		listVersion: s.listVersion,
	}, nil
}

func (s *Store) run(ctx context.Context) {
	defer close(s.done)
	var workers sync.WaitGroup
	workers.Go(func() { s.observe(ctx) })
	workers.Go(func() { s.reloadLoop(ctx, &s.policy, s.reload) })
	workers.Go(func() { s.observeNamespaces(ctx) })
	workers.Go(func() { s.reloadLoop(ctx, &s.namespaceState, s.reloadNamespaces) })
	<-ctx.Done()
	s.mu.Lock()
	s.closed = true
	s.generation = nil
	s.namespaces = nil
	s.policy.epoch++
	s.mu.Unlock()
	s.policy.queue.ShutDown()
	s.namespaceState.queue.ShutDown()
	workers.Wait()
}

func (s *Store) observe(ctx context.Context) {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
			// Always use a complete authoritative LIST, including after history loss.
			s.policy.loadMu.Lock()
			defer s.policy.loadMu.Unlock()
			s.mu.RLock()
			epoch := s.policy.epoch
			s.mu.RUnlock()
			list, err := s.list(ctx)
			if err == nil {
				if _, validationErr := parsePolicyRecords(list.Items, s.namespace); validationErr != nil {
					s.reject(epoch)
				}
			}
			return list, err
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = policyLabelKey + "=" + policyLabelValue
			return s.target.Watch(ctx, &corev1.ConfigMapList{}, client.InNamespace(s.namespace), &client.ListOptions{Raw: &options})
		},
	}
	notifications := collectionNotifications{queue: s.policy.queue, deleted: func(obj any) bool {
		if record, ok := obj.(*corev1.ConfigMap); ok && record.Name == clusterPolicyName {
			s.Invalidate()
			return true
		}
		return false
	}}
	s.observeCollection(ctx, &s.policy, lw, &corev1.ConfigMap{}, notifications)
}

func (s *Store) observeCollection(ctx context.Context, state *collectionState, lw *cache.ListWatch, object runtime.Object, notifications collectionNotifications) {
	watchFn := lw.WatchFuncWithContext
	lw.WatchFuncWithContext = func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
		s.mu.Lock()
		state.observing = false
		state.observation++
		s.mu.Unlock()
		stream, err := watchFn(ctx, options)
		if err != nil {
			s.logger.Warn("authorization watch failed", zap.String("source", state.source), zap.Error(err))
			return nil, err
		}
		s.mu.Lock()
		state.observing = true
		state.observation++
		observation := state.observation
		s.mu.Unlock()
		// Reconnection must reload even if no object events follow it.
		state.queue.Add(struct{}{})
		return &collectionWatch{Interface: stream, store: s, state: state, observation: observation}, nil
	}
	reflector := cache.NewReflectorWithOptions(authoritativeListWatch{lw}, object, notifications, cache.ReflectorOptions{Name: "authorization"})
	retry := policyRetryBackoff()
	for ctx.Err() == nil {
		started := time.Now()
		if err := reflector.ListAndWatchWithContext(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("authorization observation failed", zap.String("source", state.source), zap.Error(err))
		}
		if time.Since(started) >= time.Minute {
			retry = policyRetryBackoff()
		}
		if !waitPolicyRetry(ctx, &retry) {
			return
		}
	}
}

func (s *Store) reloadLoop(ctx context.Context, state *collectionState, reload func(context.Context) error) {
	retry := policyRetryBackoff()
	for {
		key, shutdown := state.queue.Get()
		if shutdown {
			return
		}
		if err := reload(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("authorization reload failed", zap.String("source", state.source), zap.Error(err))
			if waitPolicyRetry(ctx, &retry) {
				state.queue.Add(key)
			}
		} else {
			retry = policyRetryBackoff()
		}
		// Work arriving during the load remains dirty and is processed again.
		state.queue.Done(key)
	}
}

func (s *Store) reload(ctx context.Context) error {
	s.policy.loadMu.Lock()
	defer s.policy.loadMu.Unlock()
	s.mu.RLock()
	epoch, observation, previous := s.policy.epoch, s.policy.observation, s.generation
	s.mu.RUnlock()
	list, err := s.list(ctx)
	if err != nil {
		return err
	}
	identities := make(map[string]recordIdentity, len(list.Items))
	for _, record := range list.Items {
		identities[record.Name] = recordIdentity{uid: record.UID, version: record.ResourceVersion}
	}
	generation := previous
	if previous == nil || len(identities) != len(list.Items) || !maps.Equal(identities, previous.identities) {
		records, err := parsePolicyRecords(list.Items, s.namespace)
		if err != nil {
			s.reject(epoch)
			return err
		}
		evaluators, err := compilePolicyEvaluators(records)
		if err != nil {
			s.reject(epoch)
			return err
		}
		generation = &policyGeneration{records: records, evaluators: evaluators, identities: identities}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil || epoch != s.policy.epoch || observation != s.policy.observation || !s.policy.observing {
		return ErrPolicyUnavailable
	}
	s.generation = generation
	s.listVersion = list.ResourceVersion
	return nil
}

func (s *Store) observeNamespaces(ctx context.Context) {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
			s.namespaceState.loadMu.Lock()
			defer s.namespaceState.loadMu.Unlock()
			return s.listNamespaces(ctx)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			return s.target.Watch(ctx, &corev1.NamespaceList{}, &client.ListOptions{Raw: &options})
		},
	}
	s.observeCollection(ctx, &s.namespaceState, lw, &corev1.Namespace{}, collectionNotifications{queue: s.namespaceState.queue})
}

func (s *Store) reloadNamespaces(ctx context.Context) error {
	state := &s.namespaceState
	state.loadMu.Lock()
	defer state.loadMu.Unlock()
	s.mu.RLock()
	epoch, observation, previous := state.epoch, state.observation, s.namespaces
	s.mu.RUnlock()
	list, err := s.listNamespaces(ctx)
	if err != nil {
		return err
	}
	identities := make(map[string]recordIdentity, len(list.Items))
	for _, namespace := range list.Items {
		identities[namespace.Name] = recordIdentity{uid: namespace.UID, version: namespace.ResourceVersion}
	}
	generation := previous
	if previous == nil || !maps.Equal(identities, previous.identities) {
		generation = &namespaceGeneration{
			byName: make(map[string]*corev1.Namespace, len(list.Items)),
			names:  make([]string, 0, len(list.Items)), identities: identities,
		}
		for i := range list.Items {
			namespace := &list.Items[i]
			generation.byName[namespace.Name] = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace.Name, UID: namespace.UID,
					ResourceVersion: namespace.ResourceVersion, Labels: maps.Clone(namespace.Labels)},
				Status: *namespace.Status.DeepCopy(),
			}
			generation.names = append(generation.names, namespace.Name)
		}
		slices.Sort(generation.names)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil || epoch != state.epoch || observation != state.observation || !state.observing {
		return ErrPolicyUnavailable
	}
	s.namespaces = generation
	s.namespaceVersion = list.ResourceVersion
	return nil
}

func (s *Store) listNamespaces(ctx context.Context) (*corev1.NamespaceList, error) {
	var list corev1.NamespaceList
	if err := s.target.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("read authorization namespaces: %w", err)
	}
	if list.Continue != "" || list.ResourceVersion == "" || list.ResourceVersion == "0" {
		return nil, errors.New("authorization namespace list is incomplete or missing an authoritative version")
	}
	seen := make(map[string]bool, len(list.Items))
	for _, namespace := range list.Items {
		if namespace.Name == "" || namespace.UID == "" || namespace.ResourceVersion == "" || seen[namespace.Name] {
			return nil, errors.New("authorization namespace list contains missing or duplicate identities")
		}
		seen[namespace.Name] = true
	}
	return &list, nil
}

func (s *Store) validateNamespaces(generation *namespaceGeneration) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	select {
	case <-s.stopping:
		return ErrPolicyUnavailable
	default:
	}
	if s.closed || generation == nil || generation != s.namespaces {
		return ErrPolicyUnavailable
	}
	return nil
}

func (s *Store) list(ctx context.Context) (*corev1.ConfigMapList, error) {
	var list corev1.ConfigMapList
	if err := s.target.List(ctx, &list, client.InNamespace(s.namespace), client.MatchingLabels{policyLabelKey: policyLabelValue}); err != nil {
		return nil, fmt.Errorf("read authorization policy: %w", err)
	}
	if list.Continue != "" || list.ResourceVersion == "" || list.ResourceVersion == "0" {
		return nil, errors.New("authorization policy list is incomplete or missing an authoritative version")
	}
	return &list, nil
}

func (s *Store) reject(epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policy.epoch == epoch {
		s.generation = nil
		s.policy.epoch++
	}
}

func (s *Store) validate(generation *policyGeneration, namespaces *namespaceGeneration, needsNamespaces bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.usable(generation) || (needsNamespaces && (namespaces == nil || namespaces != s.namespaces)) {
		return ErrPolicyUnavailable
	}
	return nil
}

// usable requires mu to be held and never uses watch health or policy age.
func (s *Store) usable(generation *policyGeneration) bool {
	select {
	case <-s.stopping:
		return false
	default:
		return !s.closed && generation != nil && generation == s.generation
	}
}

func policyRetryBackoff() wait.Backoff {
	// Leave room for additive jitter so capped retries still spread out within 30 seconds.
	return wait.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.2, Steps: 10, Cap: 25 * time.Second}
}

func waitPolicyRetry(ctx context.Context, backoff *wait.Backoff) bool {
	timer := time.NewTimer(backoff.Step())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// collectionWatch marks observation as interrupted without retiring valid data.
type collectionWatch struct {
	watch.Interface
	store       *Store
	state       *collectionState
	observation uint64
}

func (w *collectionWatch) Stop() {
	w.Interface.Stop()
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	if w.state.observation == w.observation {
		w.state.observing = false
		w.state.observation++
	}
}

// collectionNotifications schedules authoritative reloads instead of caching event payloads.
type collectionNotifications struct {
	queue   workqueue.TypedInterface[struct{}]
	deleted func(any) bool
}

func (n collectionNotifications) Add(any) error {
	n.queue.Add(struct{}{})
	return nil
}

func (n collectionNotifications) Update(obj any) error { return n.Add(obj) }

func (n collectionNotifications) Delete(obj any) error {
	if n.deleted != nil && n.deleted(obj) {
		return nil
	}
	return n.Add(obj)
}

func (n collectionNotifications) Replace(_ []any, _ string) error { return n.Add(nil) }

func (n collectionNotifications) Resync() error { return nil }

// authoritativeListWatch keeps the authoritative LIST/WATCH boundary explicit instead
// of allowing the reflector to switch to streaming initial events.
type authoritativeListWatch struct{ *cache.ListWatch }

func (authoritativeListWatch) IsWatchListSemanticsUnSupported() bool { return true }

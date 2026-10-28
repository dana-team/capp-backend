package authorization

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// Store owns a target's watch-driven policy generations. Read failures preserve
// the last valid generation; invalidation, invalid policy and shutdown retire it.
// Store snapshots currently support cluster and all-namespace operations only.
type Store struct {
	target    client.WithWatch
	namespace string
	logger    *zap.Logger
	queue     workqueue.TypedInterface[struct{}]
	done      chan struct{}

	loadMu      sync.Mutex // Serializes bootstrap/recovery reads and policy reloads.
	mu          sync.RWMutex
	cancel      context.CancelFunc
	stopping    <-chan struct{}
	closed      bool
	observing   bool
	observation uint64
	epoch       uint64
	generation  *policyGeneration
	listVersion string
}

type policyGeneration struct {
	evaluators *policyEvaluators
	records    *policyRecords
	identities map[string]policyRecordIdentity
	versions   map[string]string
}

type policyRecordIdentity struct {
	uid     types.UID
	version string
}

// NewStore prepares a policy store without starting background work. The target
// must be a direct reusable service-account client, not an informer-backed client.
// The logger should identify the target cluster.
func NewStore(target client.WithWatch, cfg config.ClusterAuthorizationConfig, logger *zap.Logger) (*Store, error) {
	if target == nil || cfg.PolicyNamespace == "" || logger == nil {
		return nil, errors.New("policy store requires a client, policy namespace and logger")
	}
	return &Store{
		target: target, namespace: cfg.PolicyNamespace, logger: logger,
		queue: workqueue.NewTyped[struct{}](), done: make(chan struct{}),
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
		s.epoch++
		if s.cancel == nil {
			close(s.done)
		}
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.queue.ShutDown()
	<-s.done
}

// Invalidate immediately retires captured policy and schedules an authoritative
// reload. Work begun before invalidation cannot restore the retired generation.
func (s *Store) Invalidate() {
	s.mu.Lock()
	s.epoch++
	s.generation = nil
	s.mu.Unlock()
	s.queue.Add(struct{}{})
}

// Snapshot captures policy and verified subjects without reading Kubernetes or
// compiling evaluators. Changed publication or invalidation retires this snapshot.
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
	generation := s.generation
	return &Snapshot{
		store: s, generation: generation, evaluators: generation.evaluators,
		records: generation.records, subjects: subjects,
		resourceVersions: generation.versions, listVersion: s.listVersion,
	}, nil
}

func (s *Store) run(ctx context.Context) {
	defer close(s.done)
	var workers sync.WaitGroup
	workers.Go(func() { s.observe(ctx) })
	workers.Go(func() { s.reloadLoop(ctx) })
	<-ctx.Done()
	s.mu.Lock()
	s.closed = true
	s.generation = nil
	s.epoch++
	s.mu.Unlock()
	s.queue.ShutDown()
	workers.Wait()
}

func (s *Store) observe(ctx context.Context) {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
			// Always use a complete authoritative LIST, including after history loss.
			s.loadMu.Lock()
			defer s.loadMu.Unlock()
			s.mu.RLock()
			epoch := s.epoch
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
			s.mu.Lock()
			s.observing = false
			s.observation++
			s.mu.Unlock()
			options.LabelSelector = policyLabelKey + "=" + policyLabelValue
			stream, err := s.target.Watch(ctx, &corev1.ConfigMapList{}, client.InNamespace(s.namespace), &client.ListOptions{Raw: &options})
			if err != nil {
				s.logger.Warn("authorization policy watch failed", zap.Error(err))
				return nil, err
			}
			s.mu.Lock()
			s.observing = true
			s.observation++
			observation := s.observation
			s.mu.Unlock()
			// Reconnection must reload even if no object events follow it.
			s.queue.Add(struct{}{})
			return &policyWatch{Interface: stream, store: s, observation: observation}, nil
		},
	}
	reflector := cache.NewReflectorWithOptions(policyListWatch{lw}, &corev1.ConfigMap{}, policyNotifications{store: s}, cache.ReflectorOptions{Name: "authorization-policy"})
	retry := policyRetryBackoff()
	for ctx.Err() == nil {
		started := time.Now()
		if err := reflector.ListAndWatchWithContext(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("authorization policy observation failed", zap.Error(err))
		}
		if time.Since(started) >= time.Minute {
			retry = policyRetryBackoff()
		}
		if !waitPolicyRetry(ctx, &retry) {
			return
		}
	}
}

func (s *Store) reloadLoop(ctx context.Context) {
	retry := policyRetryBackoff()
	for {
		key, shutdown := s.queue.Get()
		if shutdown {
			return
		}
		if err := s.reload(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("authorization policy reload failed", zap.Error(err))
			if waitPolicyRetry(ctx, &retry) {
				s.queue.Add(key)
			}
		} else {
			retry = policyRetryBackoff()
		}
		// Work arriving during the load remains dirty and is processed again.
		s.queue.Done(key)
	}
}

func (s *Store) reload(ctx context.Context) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.mu.RLock()
	epoch, observation, previous := s.epoch, s.observation, s.generation
	s.mu.RUnlock()
	list, err := s.list(ctx)
	if err != nil {
		return err
	}
	identities := make(map[string]policyRecordIdentity, len(list.Items))
	for _, record := range list.Items {
		identities[record.Name] = policyRecordIdentity{uid: record.UID, version: record.ResourceVersion}
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
		versions := make(map[string]string, len(identities))
		for name, identity := range identities {
			versions[name] = identity.version
		}
		generation = &policyGeneration{records: records, evaluators: evaluators, identities: identities, versions: versions}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil || epoch != s.epoch || observation != s.observation || !s.observing {
		return ErrPolicyUnavailable
	}
	s.generation = generation
	s.listVersion = list.ResourceVersion
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
	if s.epoch == epoch {
		s.generation = nil
		s.epoch++
	}
}

func (s *Store) validate(generation *policyGeneration) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.usable(generation) {
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

// policyWatch marks observation as interrupted when the reflector closes a
// stream, without retiring the last valid policy or affecting a newer stream.
type policyWatch struct {
	watch.Interface
	store       *Store
	observation uint64
}

func (w *policyWatch) Stop() {
	w.Interface.Stop()
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	if w.store.observation == w.observation {
		w.store.observing = false
		w.store.observation++
	}
}

// policyNotifications implements the reflector's sink without caching event
// payloads. Events only schedule complete authoritative policy reloads.
type policyNotifications struct{ store *Store }

func (n policyNotifications) Add(any) error {
	n.store.queue.Add(struct{}{})
	return nil
}

func (n policyNotifications) Update(obj any) error { return n.Add(obj) }

func (n policyNotifications) Delete(obj any) error {
	if record, ok := obj.(*corev1.ConfigMap); ok && record.Name == clusterPolicyName {
		n.store.Invalidate()
		return nil
	}
	return n.Add(obj)
}

func (n policyNotifications) Replace(_ []any, _ string) error { return n.Add(nil) }

func (n policyNotifications) Resync() error { return nil }

// policyListWatch keeps the authoritative LIST/WATCH boundary explicit instead
// of allowing the reflector to switch to streaming initial events.
type policyListWatch struct{ *cache.ListWatch }

func (policyListWatch) IsWatchListSemanticsUnSupported() bool { return true }

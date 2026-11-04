package authorization

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	policies = iota
	namespaces
	collectionCount
)

var collectionNames = [collectionCount]string{"assignments", "namespaces"}

// StoreOptions configures one target cluster's observations, independently of
// server configuration. OnChange is called after publication; it should enqueue
// work promptly, must not block, and must be safe for concurrent calls.
type StoreOptions struct {
	PolicyNamespace               string
	ProtectedNamespaces           []string
	RestrictedBindingNamePrefixes []string
	OnChange                      func(namespace string)
}

// ObservationHealth separates initial synchronization from transient failures.
// Degraded contains bounded collection names, never upstream error contents.
type ObservationHealth struct {
	Synced   bool
	Degraded []string
}

// Store observes one cluster and publishes detached, indexed namespace state.
// Construct a separate Store per cluster and replica. Run owns all workers.
type Store struct {
	mu            sync.RWMutex
	options       StoreOptions
	informers     [collectionCount]cache.SharedIndexInformer
	syncedChecks  []cache.InformerSynced
	started       bool
	hasReconciler bool
	synced        bool
	degraded      [collectionCount]bool
	states        map[string]NamespaceState
	identities    map[identity]map[string]struct{}
}

type identity struct{ kind, name string }

// NewStore prepares collection informers without making API calls.
func NewStore(client kubernetes.Interface, options StoreOptions) (*Store, error) {
	if client == nil || len(validation.ValidateNamespaceName(options.PolicyNamespace, false)) != 0 {
		return nil, errors.New("observation requires a client and valid policy namespace")
	}
	if err := ValidateBindingNamePrefixes(options.RestrictedBindingNamePrefixes); err != nil {
		return nil, err
	}
	options.RestrictedBindingNamePrefixes = slices.Clone(options.RestrictedBindingNamePrefixes)
	options.ProtectedNamespaces = slices.Clone(options.ProtectedNamespaces)
	s := &Store{options: options, states: make(map[string]NamespaceState), identities: make(map[identity]map[string]struct{})}
	sources := collectionSources(client, options.PolicyNamespace)
	for i, source := range sources {
		lw := &cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
				result, err := source.list(ctx, opts)
				s.setDegraded(i, err != nil)
				return result, err
			},
			WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
				result, err := source.watch(ctx, opts)
				s.setDegraded(i, err != nil)
				return result, err
			},
		}
		informer := cache.NewSharedIndexInformer(cache.ToListWatcherWithWatchListSemantics(lw, client), source.object, 0, cache.Indexers{})
		s.informers[i] = informer
		if err := informer.SetWatchErrorHandlerWithContext(func(ctx context.Context, _ *cache.Reflector, err error) {
			s.handleWatchError(ctx, i, err)
		}); err != nil {
			return nil, err
		}
		registration, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { s.changed(i, obj) },
			UpdateFunc: func(_, current any) { s.changed(i, current) },
			DeleteFunc: func(obj any) { s.changed(i, obj) },
		})
		if err != nil {
			return nil, err
		}
		s.syncedChecks = append(s.syncedChecks, registration.HasSynced)
	}
	return s, nil
}

// Run starts all observation workers, waits for cancellation, and joins them.
// A Store is single-use; create a new instance after shutdown.
func (s *Store) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("observation store already started")
	}
	s.started = true
	s.mu.Unlock()
	var workers sync.WaitGroup
	for _, informer := range s.informers {
		workers.Go(func() { informer.RunWithContext(ctx) })
	}
	if cache.WaitForCacheSync(ctx.Done(), s.syncedChecks...) {
		s.mu.Lock()
		s.synced = true
		// Initial callbacks ran before readiness was available. Notify consumers
		// again after synchronization so queued work can use the published state.
		names := make([]string, 0, len(s.states))
		for name := range s.states {
			names = append(names, name)
		}
		s.mu.Unlock()
		if s.options.OnChange != nil {
			for _, name := range names {
				s.options.OnChange(name)
			}
		}
	}
	<-ctx.Done()
	workers.Wait()
	s.mu.Lock()
	s.synced = false
	s.mu.Unlock()
	return nil
}

// Health returns a detached summary of observation availability.
func (s *Store) Health() ObservationHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := ObservationHealth{Synced: s.synced}
	for i, degraded := range s.degraded {
		if degraded {
			h.Degraded = append(h.Degraded, collectionNames[i])
		}
	}
	return h
}

// Lookup returns the last observed state, with initial unavailability overriding
// readiness until every collection and its handlers have synchronized.
func (s *Store) Lookup(namespace string) NamespaceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.states[namespace]
	if !ok {
		state = state.condition(PolicyInvalid)
	}
	if !s.synced {
		state = state.condition(CacheNotReady)
	}
	return state.clone()
}

// Eligible returns ready assignments with an allowed role for a verified identity.
// Identity indexes narrow the candidates; each candidate still needs a role match.
// The bool distinguishes complete empty results from incomplete observations.
// Eligibility includes the managed-namespace label and protected exclusions.
func (s *Store) Eligible(username string, groups, allowedRoles []string) ([]NamespaceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.synced {
		return nil, false
	}
	keys := []identity{{kind: rbacv1.UserKind, name: username}}
	for _, group := range groups {
		keys = append(keys, identity{kind: rbacv1.GroupKind, name: group})
	}
	seen := make(map[string]struct{})
	var names []string
	for _, key := range keys {
		for name := range s.identities[key] {
			if _, exists := seen[name]; !exists && s.states[name].Ready() && s.states[name].Allows(username, groups, allowedRoles) {
				names = append(names, name)
				seen[name] = struct{}{}
			}
		}
	}
	sort.Strings(names)
	result := make([]NamespaceState, 0, len(names))
	for _, name := range names {
		result = append(result, s.states[name].clone())
	}
	return result, true
}

func (s *Store) handleWatchError(ctx context.Context, collection int, err error) {
	s.setDegraded(collection, true)
	klog.FromContext(ctx).Error(nil, "authorization observation failed", "collection", collectionNames[collection], "reason", authorizationErrorReason(err))
}

// Use bounded categories because upstream messages may contain identities or policy data.
func authorizationErrorReason(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	default:
		return "unavailable"
	}
}

func (s *Store) setDegraded(collection int, degraded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.degraded[collection] = degraded
}

func (s *Store) changed(collection int, obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	s.mu.Lock()
	targets := s.targets(collection, obj)
	for _, namespace := range targets {
		s.publish(namespace)
	}
	s.mu.Unlock()
	if s.options.OnChange != nil {
		for _, namespace := range targets {
			s.options.OnChange(namespace)
		}
	}
}

func (s *Store) targets(collection int, obj any) []string {
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil
	}
	switch collection {
	case policies:
		if strings.HasPrefix(m.GetName(), assignmentNamePrefix) {
			return []string{strings.TrimPrefix(m.GetName(), assignmentNamePrefix)}
		}
	case namespaces:
		if _, ok := s.states[m.GetName()]; ok {
			return []string{m.GetName()}
		}
	}
	return nil
}

func (s *Store) publish(namespace string) {
	old := s.states[namespace]
	s.index(namespace, old.assignment, false)
	state, exists := s.observe(namespace)
	if !exists {
		delete(s.states, namespace)
		return
	}
	s.states[namespace] = state
	s.index(namespace, state.assignment, true)
}

func (s *Store) index(namespace string, assignment *Assignment, add bool) {
	if assignment == nil {
		return
	}
	for _, b := range assignment.bindings {
		key := identity{kind: b.Kind, name: b.Name}
		if add {
			if s.identities[key] == nil {
				s.identities[key] = make(map[string]struct{})
			}
			s.identities[key][namespace] = struct{}{}
		} else {
			delete(s.identities[key], namespace)
			if len(s.identities[key]) == 0 {
				delete(s.identities, key)
			}
		}
	}
}

func (s *Store) observe(namespace string) (NamespaceState, bool) {
	state := NamespaceState{}
	cm, _ := s.object(policies, s.options.PolicyNamespace+"/"+assignmentNamePrefix+namespace).(*corev1.ConfigMap)
	if cm == nil {
		return state, false
	}
	state.PolicyUID, state.PolicyVersion = cm.UID, cm.ResourceVersion
	assignment, err := ParseAssignment(cm, s.options.PolicyNamespace, s.options.ProtectedNamespaces, s.options.RestrictedBindingNamePrefixes)
	if err != nil || cm.DeletionTimestamp != nil || cm.UID == "" {
		return state.condition(PolicyInvalid), true
	}
	state.assignment = assignment
	ns, _ := s.object(namespaces, namespace).(*corev1.Namespace)
	state.Namespace = ns
	if ns == nil || ns.DeletionTimestamp != nil || ns.UID == "" {
		return state.condition(NamespaceUnavailable), true
	}
	if ns.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedLabelValue {
		return state.condition(NamespaceUnmanaged), true
	}
	associated := cm.Annotations[NamespaceUIDAnnotation]
	if associated != "" && associated != string(ns.UID) {
		return state.condition(NamespaceUIDMismatch), true
	}
	if associated == "" {
		return state.condition(AssociationPending), true
	}
	return state.condition(Ready), true
}

func (s *Store) object(collection int, key string) any {
	obj, _, err := s.informers[collection].GetIndexer().GetByKey(key)
	if err != nil {
		return nil
	}
	return obj
}

type collectionSource struct {
	object runtime.Object
	list   cache.ListWithContextFunc
	watch  cache.WatchFuncWithContext
}

func collectionSources(client kubernetes.Interface, namespace string) [collectionCount]collectionSource {
	return [collectionCount]collectionSource{
		{object: &corev1.ConfigMap{},
			list: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
				opts.LabelSelector = labels.SelectorFromSet(labels.Set{assignmentLabel: backendPolicy}).String()
				return client.CoreV1().ConfigMaps(namespace).List(ctx, opts)
			},
			watch: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
				opts.LabelSelector = labels.SelectorFromSet(labels.Set{assignmentLabel: backendPolicy}).String()
				return client.CoreV1().ConfigMaps(namespace).Watch(ctx, opts)
			},
		},
		{object: &corev1.Namespace{},
			list: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
				return client.CoreV1().Namespaces().List(ctx, opts)
			},
			watch: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
				return client.CoreV1().Namespaces().Watch(ctx, opts)
			},
		},
	}
}

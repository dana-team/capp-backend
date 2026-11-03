package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

var errObservationsNotSynced = errors.New("authorization observations are not synchronized")

// Reconciler persists the initial namespace UID association on valid assignments.
// Run the store and reconciler with the same cancellation scope.
type Reconciler struct {
	client  kubernetes.Interface
	store   *Store
	queue   workqueue.TypedRateLimitingInterface[string]
	started atomic.Bool
}

// NewReconciler registers change handlers before the store starts.
// Each store supports one reconciler; replicas use independent stores and queues.
func NewReconciler(client kubernetes.Interface, store *Store) (*Reconciler, error) {
	if client == nil || store == nil {
		return nil, errors.New("reconciliation requires a client and observation store")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.started {
		return nil, errors.New("reconciliation must be connected before observation starts")
	}
	if store.hasReconciler {
		return nil, errors.New("observation store already has a reconciler")
	}
	r := &Reconciler{
		client: client,
		store:  store,
		queue: workqueue.NewTypedRateLimitingQueue(
			newReconciliationRateLimiter(),
		),
	}
	store.hasReconciler = true
	previous := store.options.OnChange
	store.options.OnChange = func(namespace string) {
		r.queue.Add(namespace)
		if previous != nil {
			previous(namespace)
		}
	}
	return r, nil
}

// Run processes namespace changes and retries failures with bounded backoff until
// cancellation, joining all workers before returning. A reconciler is single-use.
func (r *Reconciler) Run(ctx context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("reconciler already started")
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			namespace, shutdown := r.queue.Get()
			if shutdown {
				return
			}
			if err := r.reconcile(ctx, namespace); err != nil && ctx.Err() == nil {
				if !errors.Is(err, errObservationsNotSynced) && !apierrors.IsConflict(err) {
					klog.FromContext(ctx).Error(nil, "authorization reconciliation failed", "namespace", namespace, "reason", authorizationErrorReason(err))
				}
				r.queue.AddRateLimited(namespace)
			} else {
				r.queue.Forget(namespace)
			}
			r.queue.Done(namespace)
		}
	})
	<-ctx.Done()
	r.queue.ShutDown()
	workers.Wait()
	return nil
}

func (r *Reconciler) reconcile(ctx context.Context, namespace string) error {
	if !r.store.Health().Synced {
		return errObservationsNotSynced
	}

	state := r.store.Lookup(namespace)
	if state.Reason != AssociationPending {
		return nil
	}
	// Bound the whole attempt so a stalled API call cannot monopolize the worker.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Re-read before writes: observations may lag policy edits or replacement.
	cm, err := r.client.CoreV1().ConfigMaps(r.store.options.PolicyNamespace).Get(
		ctx, assignmentNamePrefix+namespace, metav1.GetOptions{},
	)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if cm.UID != state.PolicyUID {
		return nil
	}
	if _, err := ParseAssignment(cm, r.store.options.PolicyNamespace, r.store.options.ProtectedNamespaces, r.store.options.RestrictedBindingNamePrefixes); err != nil {
		return nil
	}
	if cm.DeletionTimestamp != nil {
		return nil
	}

	ns, err := r.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.DeletionTimestamp != nil || ns.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedLabelValue {
		return nil
	}

	if cm.Annotations[namespaceUIDAnnotation] != "" {
		return nil
	}
	return r.associateNamespace(ctx, cm, ns)
}

func (r *Reconciler) associateNamespace(ctx context.Context, cm *corev1.ConfigMap, ns *corev1.Namespace) error {
	// A metadata-only patch works for immutable ConfigMaps, preserves operator
	// fields, and cannot overwrite an association written by another replica.
	patch := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			UID:             cm.UID,
			ResourceVersion: cm.ResourceVersion,
			Annotations: map[string]string{
				namespaceUIDAnnotation: string(ns.UID),
			},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = r.client.CoreV1().ConfigMaps(cm.Namespace).Patch(
		ctx, cm.Name, types.MergePatchType, data, metav1.PatchOptions{},
	)
	return err
}

func newReconciliationRateLimiter() workqueue.TypedRateLimiter[string] {
	return workqueue.NewTypedMaxOfRateLimiter(
		// Bound per-namespace recovery backoff while retaining overall retry throttling.
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, time.Minute),
		&workqueue.TypedBucketRateLimiter[string]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

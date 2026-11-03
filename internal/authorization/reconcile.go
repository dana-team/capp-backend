package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const retirementRecheckInterval = 30 * time.Second

// Reconciler provisions and retires owned namespace bindings using a Store's
// observations. Run the store and reconciler with the same cancellation scope.
// Neither component owns the shared ServiceAccount or ClusterRole.
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
	// Index cached bindings by namespace so cleanup can find them without a policy.
	if err := store.informers[bindings].AddIndexers(cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	}); err != nil {
		return nil, err
	}
	r := &Reconciler{
		client: client,
		store:  store,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
	}
	_, err := store.informers[bindings].AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    r.bindingChanged,
		UpdateFunc: func(_, obj any) { r.bindingChanged(obj) },
		DeleteFunc: r.bindingChanged,
	})
	if err != nil {
		r.queue.ShutDown()
		return nil, err
	}
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

func (r *Reconciler) bindingChanged(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	rb, ok := obj.(*rbacv1.RoleBinding)
	if ok && rb.Labels[managedByLabel] == saRBAC {
		r.queue.Add(rb.Namespace)
	}
}

func (r *Reconciler) reconcile(ctx context.Context, namespace string) error {
	if !r.store.Health().Synced {
		return errors.New("authorization observations are not synchronized")
	}
	if err := r.retireBindings(ctx, namespace); err != nil {
		return err
	}

	state := r.store.Lookup(namespace)
	if state.Reason != ProvisioningPending && state.Reason != ManagedObjectDrift {
		return nil
	}
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
	if _, err := ParseAssignment(cm, r.store.options.PolicyNamespace, r.store.options.ProtectedNamespaces); err != nil {
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
	if ns.DeletionTimestamp != nil || ns.UID == "" || ns.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedLabelValue {
		return nil
	}

	associated := cm.Annotations[namespaceUIDAnnotation]
	if associated != "" && associated != string(ns.UID) {
		return nil
	}
	if associated == "" {
		// Wait for the store to observe the saved association before provisioning.
		return r.associateNamespace(ctx, cm, ns)
	}
	return r.provision(ctx, cm, ns)
}

func (r *Reconciler) retireBindings(ctx context.Context, namespace string) error {
	objects, err := r.store.informers[bindings].GetIndexer().ByIndex(cache.NamespaceIndex, namespace)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		rb, ok := obj.(*rbacv1.RoleBinding)
		if !ok {
			continue
		}
		if err := r.retire(ctx, rb); err != nil {
			return err
		}
	}
	return nil
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

func (r *Reconciler) provision(ctx context.Context, cm *corev1.ConfigMap, ns *corev1.Namespace) error {
	desired := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      executionSAName + "-" + string(cm.UID),
			Namespace: ns.Name,
			Labels:    map[string]string{managedByLabel: saRBAC},
			Annotations: map[string]string{
				policyUIDAnnotation:       string(cm.UID),
				namespaceUIDAnnotation:    string(ns.UID),
				targetNamespaceAnnotation: ns.Name,
			},
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Namespace: cm.Namespace,
			Name:      executionSAName,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     cappUserRole,
		},
	}
	client := r.client.RbacV1().RoleBindings(ns.Name)
	current, err := client.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if !owned(current, cm, ns) || current.DeletionTimestamp != nil {
		return nil
	}
	if current.RoleRef != desired.RoleRef {
		// roleRef is immutable. Delete only the checked object version; the
		// deletion event triggers recreation with backend-owned metadata only.
		return r.deleteBinding(ctx, current)
	}
	if managedState(cm, ns, current) == Ready {
		return nil
	}
	current.Subjects = desired.Subjects
	_, err = client.Update(ctx, current, metav1.UpdateOptions{})
	return err
}

func (r *Reconciler) retire(ctx context.Context, observed *rbacv1.RoleBinding) error {
	annotations := observed.Annotations
	policyUID := types.UID(annotations[policyUIDAnnotation])
	namespaceUID := types.UID(annotations[namespaceUIDAnnotation])
	if policyUID == "" || namespaceUID == "" || observed.Name != executionSAName+"-"+string(policyUID) {
		return nil
	}
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: policyUID}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: observed.Namespace,
		UID:  namespaceUID,
	}}
	if !owned(observed, policy, namespace) {
		return nil
	}

	// Bypass the label-filtered policy cache: withdrawing a label or making an
	// assignment invalid does not establish retirement of its UID.
	cm, err := r.client.CoreV1().ConfigMaps(r.store.options.PolicyNamespace).Get(
		ctx, assignmentNamePrefix+observed.Namespace, metav1.GetOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	retired := apierrors.IsNotFound(err) || cm.UID != policyUID
	if !retired {
		ns, err := r.client.CoreV1().Namespaces().Get(ctx, observed.Namespace, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		retired = apierrors.IsNotFound(err) || ns.UID != namespaceUID || ns.DeletionTimestamp != nil
	}
	if !retired {
		if cm.Labels[assignmentLabel] != saRBAC {
			// The filtered policy watch cannot report this object's later deletion.
			// Keep checking until retirement or selection resumes.
			r.queue.AddAfter(observed.Namespace, retirementRecheckInterval)
		}
		return nil
	}

	current, err := r.client.RbacV1().RoleBindings(observed.Namespace).Get(
		ctx, observed.Name, metav1.GetOptions{},
	)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.UID != observed.UID || !owned(current, policy, namespace) {
		return nil
	}
	return r.deleteBinding(ctx, current)
}

func (r *Reconciler) deleteBinding(ctx context.Context, rb *rbacv1.RoleBinding) error {
	if rb.UID == "" || rb.ResourceVersion == "" {
		return errors.New("binding identity is incomplete")
	}
	err := r.client.RbacV1().RoleBindings(rb.Namespace).Delete(ctx, rb.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &rb.UID, ResourceVersion: &rb.ResourceVersion},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

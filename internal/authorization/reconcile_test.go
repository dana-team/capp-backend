package authorization

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestReconcilerReconcile(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*corev1.ConfigMap, *corev1.Namespace)
	}{
		{"unmanaged", func(_ *corev1.ConfigMap, ns *corev1.Namespace) { ns.Labels = nil }},
		{"policy absent from configured namespace", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Namespace = "team-a" }},
		{"invalid", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Data[policyDataKey] = "{" }},
		{"withdrawn", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Labels = nil }},
		{"replacement", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.UID = "replacement" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cm, ns, rb := readyObjects("team-a")
			cm.Annotations = nil
			store := staticStore(t, cm.DeepCopy(), ns.DeepCopy(), rb)
			tt.change(cm, ns)
			client := fake.NewClientset(cm, ns)
			r, err := NewReconciler(client, store)
			require.NoError(t, err)
			defer r.queue.ShutDown()
			require.NoError(t, r.reconcile(t.Context(), ns.Name))
			for _, action := range client.Actions() {
				require.Contains(t, []string{"get"}, action.GetVerb())
			}
		})
	}
}

func TestReconcilerRetire(t *testing.T) {
	for _, replacement := range []string{"uid", "ownership"} {
		t.Run(replacement, func(t *testing.T) {
			cm, ns, rb := readyObjects("team-a")
			rb.ResourceVersion = "1"
			store := staticStore(t, cm, ns, rb)
			current := rb.DeepCopy()
			if replacement == "uid" {
				current.UID = "replacement"
			} else {
				current.Labels = nil
			}
			client := fake.NewClientset(ns, current)
			r, err := NewReconciler(client, store)
			require.NoError(t, err)
			defer r.queue.ShutDown()
			require.NoError(t, r.retire(t.Context(), rb))
			for _, action := range client.Actions() {
				require.Equal(t, "get", action.GetVerb())
			}
		})
	}
	t.Run("failed confirmation preserves binding", func(t *testing.T) {
		cm, ns, rb := readyObjects("team-a")
		client := fake.NewClientset(ns, rb)
		client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("unavailable") })
		r, err := NewReconciler(client, staticStore(t, cm, ns, rb))
		require.NoError(t, err)
		defer r.queue.ShutDown()
		require.Error(t, r.retire(t.Context(), rb))
		require.Len(t, client.Actions(), 1)
	})
}

// This suite uses an actual API server for optimistic concurrency, immutable
// roleRef, metadata-only ConfigMap patches, and deletion preconditions.
// Run with KUBEBUILDER_ASSETS pointing to setup-envtest's binaries.
func TestReconcilerRun(t *testing.T) {
	const (
		policyNamespace  = "capp-system"
		reconcileTimeout = 5 * time.Second
		pollInterval     = 10 * time.Millisecond
	)
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run API-backed reconciliation tests")
	}
	environment := &envtest.Environment{}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	_, err = client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: policyNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	// Inject transient write failures over real transport, without simulating API semantics.
	var failCreate, failDelete atomic.Bool
	var infrastructureCalls atomic.Int64
	t.Cleanup(func() {
		require.Zero(t, infrastructureCalls.Load(), "reconciliation must not access ServiceAccounts or ClusterRoles")
	})
	replicaConfig := rest.CopyConfig(cfg)
	replicaConfig.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
		return reconcileTransport{next: next, failCreate: &failCreate, failDelete: &failDelete, infrastructureCalls: &infrastructureCalls}
	}
	replicaClient, err := kubernetes.NewForConfig(replicaConfig)
	require.NoError(t, err)
	start := func() (*Store, func()) {
		store, err := NewStore(replicaClient, StoreOptions{PolicyNamespace: policyNamespace, ProtectedNamespaces: []string{"protected"}})
		require.NoError(t, err)
		reconciler, err := NewReconciler(replicaClient, store)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		workers.Go(func() {
			if err := store.Run(ctx); err != nil {
				t.Errorf("Store.Run: %v", err)
			}
		})
		workers.Go(func() {
			if err := reconciler.Run(ctx); err != nil {
				t.Errorf("Reconciler.Run: %v", err)
			}
		})
		stop := func() { cancel(); workers.Wait() }
		t.Cleanup(stop)
		require.Eventually(t, func() bool { return store.Health().Synced }, 10*time.Second, pollInterval)
		return store, stop
	}
	first, stopFirst := start()
	second, stopSecond := start()
	waitReady := func(namespace string) {
		require.Eventually(t, func() bool { return first.Lookup(namespace).Ready() && second.Lookup(namespace).Ready() }, 10*time.Second, pollInterval)
	}
	create := func(namespace string, managed bool) (*corev1.ConfigMap, *corev1.Namespace) {
		cm, ns, _ := readyObjects(namespace)
		ns.UID, cm.UID = "", ""
		cm.Namespace = policyNamespace
		cm.Annotations = map[string]string{"operator": "preserve"}
		if !managed {
			ns.Labels = nil
		}
		ns, err = client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{})
		require.NoError(t, err)
		if namespace == "replicas" {
			cm.Immutable = new(true)
		}
		cm, err = client.CoreV1().ConfigMaps(cm.Namespace).Create(t.Context(), cm, metav1.CreateOptions{})
		require.NoError(t, err)
		return cm, ns
	}
	binding := func(cm *corev1.ConfigMap, namespace string) *rbacv1.RoleBinding {
		rb, err := client.RbacV1().RoleBindings(namespace).Get(t.Context(), executionSAName+"-"+string(cm.UID), metav1.GetOptions{})
		require.NoError(t, err)
		return rb
	}
	t.Run("replicas associate and retry without another edit", func(t *testing.T) {
		failCreate.Store(true)
		cm, ns := create("replicas", true)
		waitReady(ns.Name)
		require.False(t, failCreate.Load())
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"operator": "preserve", namespaceUIDAnnotation: string(ns.UID)}, persisted.Annotations)
		require.Equal(t, cm.Data, persisted.Data)
		require.Equal(t, Ready, managedState(persisted, ns, binding(cm, ns.Name)))
	})
	t.Run("protected and unmanaged namespaces receive no objects", func(t *testing.T) {
		for _, namespace := range []string{"protected", "unmanaged"} {
			cm, ns := create(namespace, namespace == "protected")
			reason := NamespaceUnmanaged
			if namespace == "protected" {
				reason = PolicyInvalid
			}
			require.Eventually(t, func() bool {
				return first.Lookup(namespace).Reason == reason && second.Lookup(namespace).Reason == reason
			}, reconcileTimeout, pollInterval)
			persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Empty(t, persisted.Annotations[namespaceUIDAnnotation])
			list, err := client.RbacV1().RoleBindings(ns.Name).List(t.Context(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Empty(t, list.Items)
		}
	})
	t.Run("subject repair preserves metadata", func(t *testing.T) {
		cm, ns := create("subjects", true)
		waitReady(ns.Name)
		rb := binding(cm, ns.Name)
		rb.Annotations["operator"] = "preserve"
		rb.Subjects[0].Name = "wrong"
		_, err := client.RbacV1().RoleBindings(ns.Name).Update(t.Context(), rb, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			got := binding(cm, ns.Name)
			return got.Subjects[0].Name == executionSAName && got.Annotations["operator"] == "preserve"
		}, reconcileTimeout, pollInterval)
	})
	t.Run("ownership conflict requires correction", func(t *testing.T) {
		cm, ns := create("conflict", false)
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: executionSAName + "-" + string(cm.UID), Namespace: ns.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "unrelated"}}
		_, err := client.RbacV1().RoleBindings(ns.Name).Create(t.Context(), rb, metav1.CreateOptions{})
		require.NoError(t, err)
		ns.Labels = map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}
		_, err = client.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return first.Lookup(ns.Name).Reason == OwnershipConflict && second.Lookup(ns.Name).Reason == OwnershipConflict
		}, reconcileTimeout, pollInterval)
		require.Equal(t, rb.RoleRef, binding(cm, ns.Name).RoleRef)
		require.NoError(t, client.RbacV1().RoleBindings(ns.Name).Delete(t.Context(), rb.Name, metav1.DeleteOptions{}))
		waitReady(ns.Name)
	})
	t.Run("label withdrawal preserves bindings and association", func(t *testing.T) {
		cm, ns := create("withdrawn", true)
		waitReady(ns.Name)
		rb := binding(cm, ns.Name)
		ns.Labels = nil
		_, err := client.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return first.Lookup(ns.Name).Reason == NamespaceUnmanaged }, reconcileTimeout, pollInterval)
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		persisted.Labels = nil
		_, err = client.CoreV1().ConfigMaps(cm.Namespace).Update(t.Context(), persisted, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return first.Lookup(ns.Name).Reason == PolicyInvalid }, reconcileTimeout, pollInterval)
		require.Equal(t, rb.UID, binding(cm, ns.Name).UID)
		require.Equal(t, string(ns.UID), persisted.Annotations[namespaceUIDAnnotation])
		require.NoError(t, client.CoreV1().ConfigMaps(cm.Namespace).Delete(t.Context(), cm.Name, metav1.DeleteOptions{}))
		require.Eventually(t, func() bool {
			_, err := client.RbacV1().RoleBindings(ns.Name).Get(t.Context(), rb.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 40*time.Second, 50*time.Millisecond, "withdrawn policy deletion must retire its binding without restart")
	})
	t.Run("invalid edit denies access without retirement", func(t *testing.T) {
		_, unaffected := create("unaffected", true)
		waitReady(unaffected.Name)
		cm, ns := create("invalid-edit", true)
		waitReady(ns.Name)
		rb := binding(cm, ns.Name)
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		persisted.Data[policyDataKey] = "{"
		_, err = client.CoreV1().ConfigMaps(cm.Namespace).Update(t.Context(), persisted, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return first.Lookup(ns.Name).Reason == PolicyInvalid && second.Lookup(ns.Name).Reason == PolicyInvalid
		}, reconcileTimeout, pollInterval)
		require.Equal(t, rb.UID, binding(cm, ns.Name).UID)
		require.True(t, first.Lookup(unaffected.Name).Ready(), "other namespaces remain eligible")
	})
	t.Run("namespace recreation requires policy recreation", func(t *testing.T) {
		cm, ns := create("recreated", true)
		waitReady(ns.Name)
		require.NoError(t, client.CoreV1().Namespaces().Delete(t.Context(), ns.Name, metav1.DeleteOptions{}))
		require.Eventually(t, func() bool {
			list, err := client.RbacV1().RoleBindings(ns.Name).List(t.Context(), metav1.ListOptions{})
			return err == nil && len(list.Items) == 0
		}, reconcileTimeout, pollInterval)
		terminating, err := client.CoreV1().Namespaces().Get(t.Context(), ns.Name, metav1.GetOptions{})
		require.NoError(t, err)
		// envtest has no namespace controller; complete its finalization explicitly.
		terminating.Spec.Finalizers = nil
		_, err = client.CoreV1().Namespaces().Finalize(t.Context(), terminating, metav1.UpdateOptions{})
		require.NoError(t, err)
		replacement := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name, Labels: ns.Labels}}
		replacement, err = client.CoreV1().Namespaces().Create(t.Context(), replacement, metav1.CreateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return first.Lookup(ns.Name).Reason == NamespaceUIDMismatch && second.Lookup(ns.Name).Reason == NamespaceUIDMismatch
		}, reconcileTimeout, pollInterval)
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, string(ns.UID), persisted.Annotations[namespaceUIDAnnotation])
		require.NotEqual(t, ns.UID, replacement.UID)
		list, err := client.RbacV1().RoleBindings(ns.Name).List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Empty(t, list.Items)
	})
	// Stop both replicas to seed startup-only drift and orphan cases.
	stopFirst()
	stopSecond()
	t.Run("UID write conflict retains the first association", func(t *testing.T) {
		cm, ns := create("uid-conflict", true)
		store, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
		require.NoError(t, err)
		require.NoError(t, store.informers[policies].GetIndexer().Add(cm))
		require.NoError(t, store.informers[namespaces].GetIndexer().Add(ns))
		store.synced = true
		store.changed(policies, cm)
		conflictConfig := rest.CopyConfig(cfg)
		conflictConfig.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
			return reconcileTransport{next: next, before: func(request *http.Request) {
				if request.Method != http.MethodPatch {
					return
				}
				competing := cm.DeepCopy()
				competing.Annotations[namespaceUIDAnnotation] = "previous-namespace-instance"
				_, err := client.CoreV1().ConfigMaps(cm.Namespace).Update(t.Context(), competing, metav1.UpdateOptions{})
				require.NoError(t, err)
			}}
		}
		conflictClient, err := kubernetes.NewForConfig(conflictConfig)
		require.NoError(t, err)
		r, err := NewReconciler(conflictClient, store)
		require.NoError(t, err)
		defer r.queue.ShutDown()
		require.True(t, apierrors.IsConflict(r.reconcile(t.Context(), ns.Name)))
		require.NoError(t, r.reconcile(t.Context(), ns.Name))
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "previous-namespace-instance", persisted.Annotations[namespaceUIDAnnotation])
	})
	t.Run("deletion cannot remove a replacement or changed owner", func(t *testing.T) {
		cm, ns := create("delete-race", true)
		_, _, rb := readyObjects(ns.Name)
		rb.UID = ""
		rb, err = client.RbacV1().RoleBindings(ns.Name).Create(t.Context(), rb, metav1.CreateOptions{})
		require.NoError(t, err)
		stale := rb.DeepCopy()
		rb.Labels = nil
		_, err = client.RbacV1().RoleBindings(ns.Name).Update(t.Context(), rb, metav1.UpdateOptions{})
		require.NoError(t, err)
		r := &Reconciler{client: client}
		require.True(t, apierrors.IsConflict(r.deleteBinding(t.Context(), stale)))
		require.NoError(t, client.RbacV1().RoleBindings(ns.Name).Delete(t.Context(), rb.Name, metav1.DeleteOptions{}))
		rb.UID, rb.ResourceVersion = "", ""
		replacement, err := client.RbacV1().RoleBindings(ns.Name).Create(t.Context(), rb, metav1.CreateOptions{})
		require.NoError(t, err)
		require.True(t, apierrors.IsConflict(r.deleteBinding(t.Context(), stale)))
		got, err := client.RbacV1().RoleBindings(ns.Name).Get(t.Context(), replacement.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, replacement.UID, got.UID)
		// Remove the policy so the restarted workers only encounter this unrelated object.
		require.NoError(t, client.CoreV1().ConfigMaps(cm.Namespace).Delete(t.Context(), cm.Name, metav1.DeleteOptions{}))
	})
	t.Run("restart repairs immutable drift and cleans orphans", func(t *testing.T) {
		cm, ns := create("restart", true)
		cm.Annotations[namespaceUIDAnnotation] = string(ns.UID)
		cm, err = client.CoreV1().ConfigMaps(cm.Namespace).Update(t.Context(), cm, metav1.UpdateOptions{})
		require.NoError(t, err)
		_, _, rb := readyObjects(ns.Name)
		rb.Name, rb.UID = executionSAName+"-"+string(cm.UID), ""
		rb.Annotations[policyUIDAnnotation] = string(cm.UID)
		rb.Annotations[namespaceUIDAnnotation] = string(ns.UID)
		rb.RoleRef.Name = "wrong-role"
		rb.Labels["owner"] = "platform"
		rb.Annotations["operator"] = "preserve"
		rb, err = client.RbacV1().RoleBindings(ns.Name).Create(t.Context(), rb, metav1.CreateOptions{})
		require.NoError(t, err)
		orphan := rb.DeepCopy()
		orphan.Name, orphan.UID, orphan.ResourceVersion = executionSAName+"-retired", "", ""
		orphan.Annotations[policyUIDAnnotation] = "retired"
		_, err = client.RbacV1().RoleBindings(ns.Name).Create(t.Context(), orphan, metav1.CreateOptions{})
		require.NoError(t, err)
		failDelete.Store(true)
		failCreate.Store(true)
		first, _ = start()
		second, _ = start()
		waitReady(ns.Name)
		require.False(t, failDelete.Load())
		require.False(t, failCreate.Load())
		repaired := binding(cm, ns.Name)
		require.NotEqual(t, rb.UID, repaired.UID)
		require.Equal(t, Ready, managedState(cm, ns, repaired))
		require.Empty(t, repaired.Labels["owner"])
		require.Empty(t, repaired.Annotations["operator"])
		require.Eventually(t, func() bool {
			_, err := client.RbacV1().RoleBindings(ns.Name).Get(t.Context(), orphan.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, reconcileTimeout, pollInterval)
		require.NoError(t, client.CoreV1().ConfigMaps(cm.Namespace).Delete(t.Context(), cm.Name, metav1.DeleteOptions{}))
		cm.UID, cm.ResourceVersion, cm.Annotations = "", "", nil
		replacement, err := client.CoreV1().ConfigMaps(cm.Namespace).Create(t.Context(), cm, metav1.CreateOptions{})
		require.NoError(t, err)
		waitReady(ns.Name)
		require.NotEqual(t, rb.Name, binding(replacement, ns.Name).Name)
		require.Eventually(t, func() bool {
			_, err := client.RbacV1().RoleBindings(ns.Name).Get(t.Context(), rb.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, reconcileTimeout, pollInterval)
	})
}

type reconcileTransport struct {
	before                 func(*http.Request)
	next                   http.RoundTripper
	failCreate, failDelete *atomic.Bool
	infrastructureCalls    *atomic.Int64
}

func (r reconcileTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if r.before != nil {
		r.before(request)
		return r.next.RoundTrip(request)
	}
	if strings.Contains(request.URL.Path, "/serviceaccounts") || strings.Contains(request.URL.Path, "/clusterroles") {
		r.infrastructureCalls.Add(1)
	}
	if strings.Contains(request.URL.Path, "/rolebindings") &&
		((request.Method == http.MethodPost && r.failCreate.CompareAndSwap(true, false)) ||
			(request.Method == http.MethodDelete && r.failDelete.CompareAndSwap(true, false))) {
		return nil, errors.New("injected temporary binding write failure")
	}
	return r.next.RoundTrip(request)
}

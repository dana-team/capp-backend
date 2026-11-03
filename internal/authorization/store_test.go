package authorization

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func TestStoreLookup(t *testing.T) {
	tests := []struct {
		name   string
		change func(*corev1.ConfigMap, *corev1.Namespace, *rbacv1.RoleBinding)
		reason Reason
	}{
		{"ready", func(*corev1.ConfigMap, *corev1.Namespace, *rbacv1.RoleBinding) {
		}, Ready},
		{"invalid policy", func(cm *corev1.ConfigMap, _ *corev1.Namespace, _ *rbacv1.RoleBinding) {
			cm.Data["policy.json"] = "{"
		}, PolicyInvalid},
		{"withdrawn label", func(cm *corev1.ConfigMap, _ *corev1.Namespace, _ *rbacv1.RoleBinding) {
			cm.Labels = nil
		}, PolicyInvalid},
		{"namespace terminating", func(_ *corev1.ConfigMap, ns *corev1.Namespace, _ *rbacv1.RoleBinding) {
			now := metav1.Now()
			ns.DeletionTimestamp = &now
		}, NamespaceUnavailable},
		{"namespace label missing", func(_ *corev1.ConfigMap, ns *corev1.Namespace, _ *rbacv1.RoleBinding) {
			ns.Labels = nil
		}, NamespaceUnmanaged},
		{"namespace label not enabled", func(_ *corev1.ConfigMap, ns *corev1.Namespace, _ *rbacv1.RoleBinding) {
			ns.Labels[consts.ManagedNameSpaceLabelKey] = "false"
		}, NamespaceUnmanaged},
		{"replacement namespace", func(_ *corev1.ConfigMap, ns *corev1.Namespace, _ *rbacv1.RoleBinding) {
			ns.UID = "replacement"
		}, NamespaceUIDMismatch},
		{"association pending", func(cm *corev1.ConfigMap, _ *corev1.Namespace, _ *rbacv1.RoleBinding) {
			cm.Annotations = nil
		}, ProvisioningPending},
		{"subject drift", func(_ *corev1.ConfigMap, _ *corev1.Namespace, rb *rbacv1.RoleBinding) {
			rb.Subjects[0].Name = "another"
		}, ManagedObjectDrift},
		{"roleRef drift", func(_ *corev1.ConfigMap, _ *corev1.Namespace, rb *rbacv1.RoleBinding) {
			rb.RoleRef.Name = "another"
		}, ManagedObjectDrift},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm, ns, rb := readyObjects("team-a")
			tt.change(cm, ns, rb)
			s := staticStore(t, cm, ns, rb)
			state := s.Lookup(ns.Name)
			require.Equal(t, tt.reason, state.Reason)
			require.Equal(t, tt.reason == Ready, state.Ready())
			require.NotEmpty(t, state.Message())
		})
	}
}

func TestStoreEligible(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	s := staticStore(t, cm, ns, rb)
	got, complete := s.Eligible("alice", []string{"developers", "developers"})
	require.True(t, complete)
	require.Len(t, got, 1)
	got[0].Namespace.Labels["changed"] = "true"
	got[0].assignment.bindings[0].Name = "someone"
	require.True(t, s.Lookup(ns.Name).Matches("alice", nil))
	require.NotContains(t, s.Lookup(ns.Name).Namespace.Labels, "changed")
	got, complete = s.Eligible("Alice", nil)
	require.True(t, complete)
	require.Empty(t, got)
	cm = cm.DeepCopy()
	cm.Data["policy.json"] = fmt.Sprintf(`{"version":1,"namespace":%q,"bindings":[{"kind":"User","name":"bob","role":"capp-user"}]}`, ns.Name)
	require.NoError(t, s.informers[policies].GetIndexer().Update(cm))
	s.changed(policies, cm)
	got, _ = s.Eligible("alice", []string{"developers"})
	require.Empty(t, got)
	got, _ = s.Eligible("bob", nil)
	require.Len(t, got, 1)
	require.NoError(t, s.informers[policies].GetIndexer().Delete(cm))
	s.changed(policies, cm)
	got, _ = s.Eligible("bob", nil)
	require.Empty(t, got)
	require.Equal(t, PolicyInvalid, s.Lookup(ns.Name).Reason)
}

func TestStoreRun(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	client := fake.NewClientset(cm, ns, rb)
	s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	var readyNotification atomic.Bool
	s.options.OnChange = func(namespace string) {
		if s.Lookup(namespace).Ready() {
			readyNotification.Store(true)
		}
	}
	require.Equal(t, CacheNotReady, s.Lookup(ns.Name).Reason)
	_, complete := s.Eligible("alice", nil)
	require.False(t, complete)
	cancel, done := runStore(t, s)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Ready() }, 5*time.Second, time.Millisecond)
	require.Eventually(t, readyNotification.Load, 5*time.Second, time.Millisecond)
	rb.Subjects[0].Name = "wrong"
	_, err = client.RbacV1().RoleBindings(ns.Name).Update(t.Context(), rb, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Reason == ManagedObjectDrift }, 5*time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	require.False(t, s.Health().Synced)
	require.Error(t, s.Run(t.Context()))
	for _, action := range client.Actions() {
		resource := action.GetResource().Resource
		if resource != "configmaps" && resource != "namespaces" && resource != "rolebindings" {
			t.Errorf("unexpected observation of %s", resource)
		}
		if action.GetVerb() != "list" && action.GetVerb() != "watch" && action.GetVerb() != "update" {
			t.Errorf("unexpected action %s on %s", action.GetVerb(), resource)
		}
	}
}

func TestStoreNamespaceRecreation(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	client := fake.NewClientset(cm, ns, rb)
	s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	_, _ = runStore(t, s)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Ready() }, 5*time.Second, time.Millisecond)
	require.NoError(t, client.CoreV1().Namespaces().Delete(t.Context(), ns.Name, metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Reason == NamespaceUnavailable }, 5*time.Second, time.Millisecond)
	replacement := ns.DeepCopy()
	replacement.UID = "replacement-namespace"
	_, err = client.CoreV1().Namespaces().Create(t.Context(), replacement, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Reason == NamespaceUIDMismatch }, 5*time.Second, time.Millisecond)
}

func TestStoreNamespaceLabelChanges(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	ns.Labels = nil
	client := fake.NewClientset(cm, ns, rb)
	s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	_, _ = runStore(t, s)
	require.Eventually(t, func() bool {
		return s.Lookup(ns.Name).Reason == NamespaceUnmanaged
	}, 5*time.Second, time.Millisecond)
	for _, enabled := range []bool{true, false, true} {
		updated := ns.DeepCopy()
		if enabled {
			updated.Labels = map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}
		}
		_, err = client.CoreV1().Namespaces().Update(t.Context(), updated, metav1.UpdateOptions{})
		require.NoError(t, err)
		want := NamespaceUnmanaged
		if enabled {
			want = Ready
		}
		require.Eventually(t, func() bool { return s.Lookup(ns.Name).Reason == want }, 5*time.Second, time.Millisecond)
		eligible, synced := s.Eligible("alice", nil)
		require.True(t, synced)
		require.Equal(t, enabled, len(eligible) == 1)
		unassigned, _ := s.Eligible("bob", nil)
		require.Empty(t, unassigned, "the namespace label does not grant a user assignment")
	}
}

func TestStoreBindingLifecycle(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	otherCM, otherNS, otherRB := readyObjects("team-b")
	client := fake.NewClientset(cm, ns, rb, otherCM, otherNS, otherRB)
	s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	_, _ = runStore(t, s)
	require.Eventually(t, func() bool {
		return s.Lookup(ns.Name).Ready() && s.Lookup(otherNS.Name).Ready()
	}, 5*time.Second, time.Millisecond)
	require.NoError(t, client.RbacV1().RoleBindings(ns.Name).Delete(t.Context(), rb.Name, metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Reason == ProvisioningPending }, 5*time.Second, time.Millisecond)
	require.True(t, s.Lookup(otherNS.Name).Ready(), "binding loss is namespace-local")
	_, err = client.RbacV1().RoleBindings(ns.Name).Create(t.Context(), rb, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Ready() }, 5*time.Second, time.Millisecond)
}

func TestStoreObservationRecovery(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	client := fake.NewClientset(cm, ns, rb)
	var fail atomic.Bool
	fail.Store(true)
	client.PrependReactor("list", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, errors.New("private upstream details")
		}
		return false, nil, nil
	})
	s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	_, _ = runStore(t, s)
	require.Eventually(t, func() bool { return len(s.Health().Degraded) > 0 }, 5*time.Second, time.Millisecond)
	require.False(t, s.Lookup(ns.Name).Ready())
	fail.Store(false)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Ready() && len(s.Health().Degraded) == 0 }, 10*time.Second, time.Millisecond)
}

func TestStoreRelistRemovals(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	client := fake.NewClientset(cm, ns, rb)
	stream := watch.NewRaceFreeFake()
	var watching, unavailable atomic.Bool
	client.PrependReactor("list", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		if unavailable.Load() {
			return true, nil, errors.New("upstream unavailable")
		}
		return false, nil, nil
	})
	client.PrependWatchReactor("configmaps", func(ktesting.Action) (bool, watch.Interface, error) {
		if watching.CompareAndSwap(false, true) {
			return true, stream, nil
		}
		return false, nil, nil
	})
	s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	_, _ = runStore(t, s)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Ready() && watching.Load() }, 5*time.Second, time.Millisecond)
	unavailable.Store(true)
	stream.Error(&metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonExpired, Code: http.StatusGone})
	require.Eventually(t, func() bool { return len(s.Health().Degraded) > 0 }, 10*time.Second, time.Millisecond)
	require.True(t, s.Lookup(ns.Name).Ready(), "temporary observation failure retains valid state")
	// Delete without delivering an event on the disconnected stream.
	require.NoError(t, client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("configmaps"), cm.Namespace, cm.Name))
	unavailable.Store(false)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Reason == PolicyInvalid && len(s.Health().Degraded) == 0 }, 10*time.Second, time.Millisecond)
	require.False(t, s.Lookup(ns.Name).Ready())
}

func TestStoreIsolation(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	s := staticStore(t, cm, ns, rb)
	other := staticStore(t, cm.DeepCopy(), ns.DeepCopy(), rb.DeepCopy())
	secondCM, secondNS, secondRB := readyObjects("team-b")
	require.NoError(t, s.informers[policies].GetIndexer().Add(secondCM))
	require.NoError(t, s.informers[namespaces].GetIndexer().Add(secondNS))
	require.NoError(t, s.informers[bindings].GetIndexer().Add(secondRB))
	s.changed(policies, secondCM)
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				s.Lookup(ns.Name)
				s.Eligible("alice", nil)
				s.Health()
			}
		})
	}
	for i := range 30 {
		changed := cm.DeepCopy()
		changed.ResourceVersion = fmt.Sprint(i)
		changed.Data["policy.json"] = "{"
		require.NoError(t, s.informers[policies].GetIndexer().Update(changed))
		s.changed(policies, changed)
	}
	readers.Wait()
	require.False(t, s.Lookup(ns.Name).Ready())
	require.True(t, s.Lookup(secondNS.Name).Ready())
	require.True(t, other.Lookup(ns.Name).Ready())
}

func staticStore(t *testing.T, cm *corev1.ConfigMap, ns *corev1.Namespace, rb *rbacv1.RoleBinding) *Store {
	t.Helper()
	s, err := NewStore(fake.NewClientset(), StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	for i, obj := range []runtime.Object{cm, ns, rb} {
		require.NoError(t, s.informers[i].GetIndexer().Add(obj))
	}
	s.synced = true
	s.changed(policies, cm)
	return s
}

func runStore(t *testing.T, s *Store) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel, done
}

func readyObjects(namespace string) (*corev1.ConfigMap, *corev1.Namespace, *rbacv1.RoleBinding) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID(namespace + "-uid"), Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}}
	cm := assignmentConfigMap(fmt.Sprintf(`{"version":1,"namespace":%q,"bindings":[{"kind":"User","name":"alice","role":"capp-user"},{"kind":"Group","name":"developers","role":"capp-user"}]}`, namespace))
	cm.Name = assignmentNamePrefix + namespace
	cm.UID = types.UID(namespace + "-policy")
	cm.Annotations = map[string]string{namespaceUIDAnnotation: string(ns.UID)}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: executionSAName + "-" + string(cm.UID), Namespace: namespace, UID: "binding-uid",
			Labels:      map[string]string{managedByLabel: saRBAC},
			Annotations: map[string]string{policyUIDAnnotation: string(cm.UID), namespaceUIDAnnotation: string(ns.UID), targetNamespaceAnnotation: namespace},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cappUserRole},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: cm.Namespace, Name: executionSAName}},
	}
	return cm, ns, rb
}

func TestStoreMissingObjects(t *testing.T) {
	for _, tt := range []struct {
		name       string
		collection int
		reason     Reason
	}{
		{name: "policy", collection: policies, reason: PolicyInvalid},
		{name: "namespace", collection: namespaces, reason: NamespaceUnavailable},
		{name: "binding", collection: bindings, reason: ProvisioningPending},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cm, ns, rb := readyObjects("team-a")
			s := staticStore(t, cm, ns, rb)
			object := []runtime.Object{cm, ns, rb}[tt.collection]
			require.NoError(t, s.informers[tt.collection].GetIndexer().Delete(object))
			s.changed(tt.collection, cache.DeletedFinalStateUnknown{Obj: object})
			require.Equal(t, tt.reason, s.Lookup(ns.Name).Reason)
			require.NoError(t, s.informers[tt.collection].GetIndexer().Add(object))
			s.changed(tt.collection, object)
			require.True(t, s.Lookup(ns.Name).Ready())
		})
	}
}

func TestStoreIdentityReplacement(t *testing.T) {
	cm, ns, rb := readyObjects("team-a")
	s := staticStore(t, cm, ns, rb)
	cm = cm.DeepCopy()
	cm.UID = "new-policy"
	require.NoError(t, s.informers[policies].GetIndexer().Update(cm))
	s.changed(policies, cm)
	require.Equal(t, ProvisioningPending, s.Lookup(ns.Name).Reason)
	require.Equal(t, cm.UID, s.Lookup(ns.Name).PolicyUID)
}

func TestStoreRestrictions(t *testing.T) {
	for _, namespace := range []string{"team-a", "capp-system"} {
		cm, ns, rb := readyObjects(namespace)
		s, err := NewStore(fake.NewClientset(), StoreOptions{PolicyNamespace: "capp-system", ProtectedNamespaces: []string{"team-a"}})
		require.NoError(t, err)
		for i, obj := range []runtime.Object{cm, ns, rb} {
			require.NoError(t, s.informers[i].GetIndexer().Add(obj))
		}
		s.synced = true
		s.changed(policies, cm)
		require.False(t, s.Lookup(ns.Name).Matches("alice", nil))
		require.False(t, s.Lookup(ns.Name).Ready())
	}
}

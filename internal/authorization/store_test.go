package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

func TestStoreLookup(t *testing.T) {
	tests := []struct {
		name   string
		change func(*corev1.ConfigMap, *corev1.Namespace)
		reason Reason
	}{
		{"ready", func(*corev1.ConfigMap, *corev1.Namespace) {
		}, Ready},
		{"invalid policy", func(cm *corev1.ConfigMap, _ *corev1.Namespace) {
			cm.Data[policyDataKey] = "{"
		}, PolicyInvalid},
		{"withdrawn label", func(cm *corev1.ConfigMap, _ *corev1.Namespace) {
			cm.Labels = nil
		}, PolicyInvalid},
		{"namespace terminating", func(_ *corev1.ConfigMap, ns *corev1.Namespace) {
			now := metav1.Now()
			ns.DeletionTimestamp = &now
		}, NamespaceUnavailable},
		{"namespace label missing", func(_ *corev1.ConfigMap, ns *corev1.Namespace) {
			ns.Labels = nil
		}, NamespaceUnmanaged},
		{"namespace label not enabled", func(_ *corev1.ConfigMap, ns *corev1.Namespace) {
			ns.Labels[consts.ManagedNameSpaceLabelKey] = "false"
		}, NamespaceUnmanaged},
		{"replacement namespace", func(_ *corev1.ConfigMap, ns *corev1.Namespace) {
			ns.UID = "replacement"
		}, NamespaceUIDMismatch},
		{"association pending", func(cm *corev1.ConfigMap, _ *corev1.Namespace) {
			cm.Annotations = nil
		}, AssociationPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm, ns := readyObjects("team-a")
			tt.change(cm, ns)
			s := staticStore(t, cm, ns)
			state := s.Lookup(ns.Name)
			require.Equal(t, tt.reason, state.Reason)
			require.Equal(t, tt.reason == Ready, state.Ready())
		})
	}
}

func TestStoreLookupSnapshot(t *testing.T) {
	cm, ns := readyObjects("team-a")
	cm.ResourceVersion = "1"
	s := staticStore(t, cm, ns)
	previous := s.Lookup(ns.Name)

	updated := cm.DeepCopy()
	updated.ResourceVersion = "2"
	updated.Data[policyDataKey] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"alice","role":"editor"}]}`
	require.NoError(t, s.informers[policies].GetIndexer().Update(updated))
	s.changed(policies, updated)
	current := s.Lookup(ns.Name)

	for _, tt := range []struct {
		name    string
		state   NamespaceState
		version string
		role    string
	}{
		{name: "retained snapshot", state: previous, version: "1", role: RoleViewer},
		{name: "current snapshot", state: current, version: "2", role: RoleEditor},
	} {
		require.Equal(t, cm.UID, tt.state.PolicyUID, tt.name)
		require.Equal(t, tt.version, tt.state.PolicyResourceVersion, tt.name)
		require.NotNil(t, tt.state.Namespace, tt.name)
		require.Equal(t, ns.UID, tt.state.Namespace.UID, tt.name)
		require.True(t, tt.state.Ready(), tt.name)
		require.Equal(t, tt.role == RoleViewer, tt.state.Allows("alice", nil, []string{RoleViewer}), tt.name)
		require.Equal(t, tt.role == RoleEditor, tt.state.Allows("alice", nil, []string{RoleEditor}), tt.name)
	}
	current.Namespace.Labels[consts.ManagedNameSpaceLabelKey] = "false"
	current.assignment.bindings[0].Name = "someone"
	unchanged := s.Lookup(ns.Name)
	require.Equal(t, consts.ManagedLabelValue, unchanged.Namespace.Labels[consts.ManagedNameSpaceLabelKey])
	require.True(t, unchanged.Allows("alice", nil, []string{RoleEditor}))
}

func TestStoreEligible(t *testing.T) {
	cm, ns := readyObjects("team-a")
	s := staticStore(t, cm, ns)
	got, complete := s.Eligible("alice", []string{"developers", "developers"}, []string{RoleViewer, RoleEditor})
	require.True(t, complete)
	require.Len(t, got, 1)
	got[0].Namespace.Labels["changed"] = "true"
	for i := range got[0].assignment.bindings {
		got[0].assignment.bindings[i].Name = "someone"
	}
	require.True(t, s.Lookup(ns.Name).Allows("alice", nil, []string{RoleViewer, RoleEditor}))
	require.NotContains(t, s.Lookup(ns.Name).Namespace.Labels, "changed")
	got, _ = s.Eligible("alice", nil, []string{RoleEditor})
	require.Empty(t, got, "membership alone must not grant an editor operation")
	got, _ = s.Eligible("alice", []string{"developers"}, []string{RoleEditor})
	require.Len(t, got, 1, "a group editor grant adds to a user viewer grant")
	got, _ = s.Eligible("alice", []string{"developers"}, nil)
	require.Empty(t, got, "an absent operation role rule must grant nothing")
	got, complete = s.Eligible("Alice", nil, []string{RoleViewer, RoleEditor})
	require.True(t, complete)
	require.Empty(t, got)
	cm = cm.DeepCopy()
	cm.Data[policyDataKey] = fmt.Sprintf(`{"version":1,"namespace":%q,"bindings":[{"kind":"User","name":"bob","role":"editor"}]}`, ns.Name)
	require.NoError(t, s.informers[policies].GetIndexer().Update(cm))
	s.changed(policies, cm)
	got, _ = s.Eligible("alice", []string{"developers"}, []string{RoleViewer, RoleEditor})
	require.Empty(t, got)
	got, _ = s.Eligible("bob", nil, []string{RoleViewer, RoleEditor})
	require.Len(t, got, 1)
	require.NoError(t, s.informers[policies].GetIndexer().Delete(cm))
	s.changed(policies, cm)
	got, _ = s.Eligible("bob", nil, []string{RoleViewer, RoleEditor})
	require.Empty(t, got)
	require.Equal(t, PolicyInvalid, s.Lookup(ns.Name).Reason)
}

func TestNewStore(t *testing.T) {
	cm, ns := readyObjects("team-a")
	prefixes := []string{"system:"}
	s, err := NewStore(fake.NewClientset(), StoreOptions{PolicyNamespace: cm.Namespace, RestrictedBindingNamePrefixes: prefixes})
	require.NoError(t, err)
	prefixes[0] = "changed:"
	s.synced = true
	require.NoError(t, s.informers[namespaces].GetIndexer().Add(ns))
	require.NoError(t, s.informers[policies].GetIndexer().Add(cm))
	s.changed(policies, cm)
	require.True(t, s.Lookup(ns.Name).Allows("alice", nil, []string{RoleViewer}))

	updated := cm.DeepCopy()
	updated.Data[policyDataKey] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"Group","name":"system:authenticated","role":"editor"}]}`
	require.NoError(t, s.informers[policies].GetIndexer().Update(updated))
	s.changed(policies, updated)
	state := s.Lookup(ns.Name)
	require.Equal(t, PolicyInvalid, state.Reason)
	require.False(t, state.Allows("alice", []string{"system:authenticated"}, []string{RoleViewer, RoleEditor}))
	eligible, complete := s.Eligible("alice", []string{"system:authenticated"}, []string{RoleViewer, RoleEditor})
	require.True(t, complete)
	require.Empty(t, eligible)

	_, err = NewStore(fake.NewClientset(), StoreOptions{PolicyNamespace: cm.Namespace, RestrictedBindingNamePrefixes: []string{""}})
	require.ErrorContains(t, err, "empty prefix")
}

func TestStoreRun(t *testing.T) {
	cm, ns := readyObjects("team-a")
	client := fake.NewClientset(cm, ns)
	s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	var readyNotification atomic.Bool
	s.options.OnChange = func(namespace string) {
		if s.Lookup(namespace).Ready() {
			readyNotification.Store(true)
		}
	}
	require.Equal(t, CacheNotReady, s.Lookup(ns.Name).Reason)
	_, complete := s.Eligible("alice", nil, []string{RoleViewer, RoleEditor})
	require.False(t, complete)
	cancel, done := runStore(t, s)
	require.Eventually(t, func() bool { return s.Lookup(ns.Name).Ready() }, 5*time.Second, time.Millisecond)
	require.Eventually(t, readyNotification.Load, 5*time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	require.False(t, s.Health().Synced)
	require.Error(t, s.Run(t.Context()))
	for _, action := range client.Actions() {
		resource := action.GetResource().Resource
		require.Contains(t, []string{"list", "watch"}, action.GetVerb(), "unexpected action on %s", resource)
		var namespace, selector string
		switch resource {
		case "configmaps":
			namespace, selector = cm.Namespace, assignmentLabel+"="+backendPolicy
		case "namespaces":
		default:
			t.Fatalf("unexpected observation of %s", resource)
		}
		require.Equal(t, namespace, action.GetNamespace(), "%s %s scope", action.GetVerb(), resource)
		switch action := action.(type) {
		case ktesting.ListAction:
			require.Equal(t, selector, action.GetListRestrictions().Labels.String(), "%s list selector", resource)
		case ktesting.WatchAction:
			require.Equal(t, selector, action.GetWatchRestrictions().Labels.String(), "%s watch selector", resource)
		default:
			t.Errorf("unexpected action %s on %s", action.GetVerb(), resource)
		}
	}
}

func TestStoreNamespaceRecreation(t *testing.T) {
	cm, ns := readyObjects("team-a")
	client := fake.NewClientset(cm, ns)
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
	cm, ns := readyObjects("team-a")
	ns.Labels = nil
	client := fake.NewClientset(cm, ns)
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
		eligible, synced := s.Eligible("alice", nil, []string{RoleViewer, RoleEditor})
		require.True(t, synced)
		require.Equal(t, enabled, len(eligible) == 1)
		unassigned, _ := s.Eligible("bob", nil, []string{RoleViewer, RoleEditor})
		require.Empty(t, unassigned, "the namespace label does not grant a user assignment")
	}
}

func TestStoreObservationRecovery(t *testing.T) {
	for _, tt := range []struct {
		resource string
		health   string
	}{
		{resource: "configmaps", health: "assignments"},
		{resource: "namespaces", health: "namespaces"},
	} {
		t.Run(tt.resource, func(t *testing.T) {
			cm, ns := readyObjects("team-a")
			client := fake.NewClientset(cm, ns)
			var fail atomic.Bool
			fail.Store(true)
			client.PrependReactor("list", tt.resource, func(ktesting.Action) (bool, runtime.Object, error) {
				if fail.Load() {
					return true, nil, errors.New("private upstream details")
				}
				return false, nil, nil
			})
			s, err := NewStore(client, StoreOptions{PolicyNamespace: cm.Namespace})
			require.NoError(t, err)
			_, _ = runStore(t, s)
			require.Eventually(t, func() bool { return len(s.Health().Degraded) > 0 }, 5*time.Second, time.Millisecond)
			require.Equal(t, ObservationHealth{Degraded: []string{tt.health}}, s.Health())
			require.Equal(t, CacheNotReady, s.Lookup(ns.Name).Reason)
			eligible, complete := s.Eligible("alice", nil, []string{RoleViewer})
			require.False(t, complete)
			require.Empty(t, eligible)
			fail.Store(false)
			require.Eventually(t, func() bool { return s.Lookup(ns.Name).Ready() && len(s.Health().Degraded) == 0 }, 10*time.Second, time.Millisecond)
		})
	}
}

func TestStoreRelistRemovals(t *testing.T) {
	cm, ns := readyObjects("team-a")
	client := fake.NewClientset(cm, ns)
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
	cm, ns := readyObjects("team-a")
	s := staticStore(t, cm, ns)
	other := staticStore(t, cm.DeepCopy(), ns.DeepCopy())
	secondCM, secondNS := readyObjects("team-b")
	require.NoError(t, s.informers[policies].GetIndexer().Add(secondCM))
	require.NoError(t, s.informers[namespaces].GetIndexer().Add(secondNS))
	s.changed(policies, secondCM)
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				s.Lookup(ns.Name)
				s.Eligible("alice", nil, []string{RoleViewer, RoleEditor})
				s.Health()
			}
		})
	}
	for i := range 30 {
		changed := cm.DeepCopy()
		changed.ResourceVersion = fmt.Sprint(i)
		changed.Data[policyDataKey] = "{"
		require.NoError(t, s.informers[policies].GetIndexer().Update(changed))
		s.changed(policies, changed)
	}
	readers.Wait()
	require.False(t, s.Lookup(ns.Name).Ready())
	require.True(t, s.Lookup(secondNS.Name).Ready())
	require.True(t, other.Lookup(ns.Name).Ready())
}

func staticStore(t *testing.T, cm *corev1.ConfigMap, ns *corev1.Namespace) *Store {
	t.Helper()
	s, err := NewStore(fake.NewClientset(), StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	for i, obj := range []runtime.Object{cm, ns} {
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

func readyObjects(namespace string) (*corev1.ConfigMap, *corev1.Namespace) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID(namespace + "-uid"), Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}}
	cm := assignmentConfigMap(fmt.Sprintf(`{"version":1,"namespace":%q,"bindings":[{"kind":"User","name":"alice","role":"viewer"},{"kind":"Group","name":"developers","role":"editor"}]}`, namespace))
	cm.Name = assignmentNamePrefix + namespace
	cm.UID = types.UID(namespace + "-policy")
	cm.Annotations = map[string]string{namespaceUIDAnnotation: string(ns.UID)}

	return cm, ns
}

func TestStoreMissingObjects(t *testing.T) {
	for _, tt := range []struct {
		name       string
		collection int
		reason     Reason
	}{
		{name: "policy", collection: policies, reason: PolicyInvalid},
		{name: "namespace", collection: namespaces, reason: NamespaceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cm, ns := readyObjects("team-a")
			s := staticStore(t, cm, ns)
			object := []runtime.Object{cm, ns}[tt.collection]
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
	cm, ns := readyObjects("team-a")
	s := staticStore(t, cm, ns)
	cm = cm.DeepCopy()
	cm.UID = "new-policy"
	cm.ResourceVersion = "new-version"
	cm.Annotations = nil
	require.NoError(t, s.informers[policies].GetIndexer().Update(cm))
	s.changed(policies, cm)
	require.Equal(t, AssociationPending, s.Lookup(ns.Name).Reason)
	require.Equal(t, cm.UID, s.Lookup(ns.Name).PolicyUID)
	require.Equal(t, cm.ResourceVersion, s.Lookup(ns.Name).PolicyResourceVersion)
}

func TestStoreRestrictions(t *testing.T) {
	for _, namespace := range []string{"team-a", "capp-system"} {
		cm, ns := readyObjects(namespace)
		s, err := NewStore(fake.NewClientset(), StoreOptions{PolicyNamespace: "capp-system", ProtectedNamespaces: []string{"team-a"}})
		require.NoError(t, err)
		for i, obj := range []runtime.Object{cm, ns} {
			require.NoError(t, s.informers[i].GetIndexer().Add(obj))
		}
		s.synced = true
		s.changed(policies, cm)
		require.False(t, s.Lookup(ns.Name).Allows("alice", nil, []string{RoleViewer, RoleEditor}))
		require.False(t, s.Lookup(ns.Name).Ready())
	}
}

func TestStoreHandleWatchError(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		collection                 int
		err                        error
		wantCollection, wantReason string
	}{
		{"wrapped forbidden", policies, fmt.Errorf("private wrapper: %w", apierrors.NewForbidden(corev1.Resource("configmaps"), "private-name", errors.New("private detail"))), "assignments", "forbidden"},
		{"unauthorized", namespaces, apierrors.NewUnauthorized("private detail"), "namespaces", "unauthorized"},
		{"other failure", policies, errors.New("private detail"), "assignments", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []string
			logger := funcr.NewJSON(func(entry string) { entries = append(entries, entry) }, funcr.Options{})
			s := &Store{}
			s.handleWatchError(klog.NewContext(t.Context(), logger), tc.collection, tc.err)
			require.Equal(t, []string{tc.wantCollection}, s.Health().Degraded)
			require.Len(t, entries, 1)
			var fields map[string]any
			require.NoError(t, json.Unmarshal([]byte(entries[0]), &fields))
			require.Equal(t, "authorization observation failed", fields["msg"])
			require.Equal(t, tc.wantCollection, fields["collection"])
			require.Equal(t, tc.wantReason, fields["reason"])
			require.NotContains(t, entries[0], "private")
		})
	}
}

package namespaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestHandlerGetManagedNamespace(t *testing.T) {
	bindings := []authorization.Binding{{Kind: rbacv1.UserKind, Name: "alice", Role: authorization.RoleEditor}, {Kind: rbacv1.UserKind, Name: "reader", Role: authorization.RoleViewer}, {Kind: rbacv1.GroupKind, Name: "team", Role: authorization.RoleEditor}}
	for _, scenario := range []string{"valid", "missing policy", "invalid policy", "terminating namespace", "unowned policy", "terminating policy", "unmanaged namespace"} {
		t.Run(scenario, func(t *testing.T) {
			ns, policy := membershipObjects(t, bindings)
			objects := []client.Object{ns, existingResourceQuota(ns.Name, "2", "4Gi")}
			switch scenario {
			case "invalid policy":
				policy.Data = map[string]string{"policy.json": "sensitive malformed body"}
			case "terminating namespace":
				ns.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				ns.Finalizers = []string{"hold"}
				ns.Status.Phase = corev1.NamespaceTerminating
			case "unowned policy":
				policy.Labels = nil
			case "terminating policy":
				policy.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				policy.Finalizers = []string{"hold"}
			case "unmanaged namespace":
				ns.Labels = nil
			}
			if scenario != "missing policy" {
				objects = append(objects, policy)
			}
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{}, objects...)
			core, logs := observer.New(zap.InfoLevel)
			router := membershipRouter(t, storage, "operator", zap.New(core), nil)
			w := membershipRequest(router, http.MethodGet, ns.Name, "")
			if scenario == "unowned policy" || scenario == "terminating policy" {
				require.Equal(t, http.StatusConflict, w.Code)
				return
			}
			if scenario == "unmanaged namespace" {
				require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
				var response struct {
					Error apierrors.APIError `json:"error"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.Equal(t, apierrors.CodeConflict, response.Error.Code)
				require.Equal(t, http.StatusConflict, response.Error.Status)
				require.Equal(t, "namespace must be managed", response.Error.Message)
				require.Empty(t, response.Error.Details)
				authorizationAudits := logs.FilterMessage("authorization decision").All()
				require.Len(t, authorizationAudits, 1)
				require.Equal(t, "allowed", authorizationAudits[0].ContextMap()["outcome"])
				readAudits := logs.FilterMessage("namespace read").All()
				require.Len(t, readAudits, 1)
				require.Equal(t, "failed", readAudits[0].ContextMap()["outcome"])
				return
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var item NamespaceItem
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
			require.Equal(t, ns.Name, item.Name)
			require.True(t, item.CanEdit)
			require.NotNil(t, item.Quota)
			require.Equal(t, "2", item.Quota.CPU)
			if scenario == "missing policy" {
				require.NotContains(t, w.Body.String(), "policyResourceVersion")
				require.NotNil(t, item.Bindings)
				require.Empty(t, *item.Bindings)
			} else {
				require.Equal(t, policy.ResourceVersion, item.PolicyResourceVersion)
				if scenario == "invalid policy" {
					require.Equal(t, "invalid", item.PolicyState)
					require.NotContains(t, w.Body.String(), "bindings")
					require.NotContains(t, w.Body.String(), "users")
				} else {
					require.Equal(t, []string{"alice"}, *item.Users)
					require.ElementsMatch(t, bindings, *item.Bindings)
				}
			}
			if scenario == "terminating namespace" {
				require.Equal(t, string(corev1.NamespaceTerminating), item.Status)
			}
			entries := logs.FilterMessage("namespace read").All()
			require.Len(t, entries, 1)
			require.Equal(t, "operator", entries[0].ContextMap()["caller"])
			require.Equal(t, "succeeded", entries[0].ContextMap()["outcome"])
			require.NotContains(t, fmt.Sprint(entries), "sensitive malformed body")
		})
	}
}

func TestHandlerUpdateManagedNamespaceRevisionRoundTrip(t *testing.T) {
	ns, policy := membershipObjects(t, []authorization.Binding{})
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{}, ns, policy)
	router := membershipRouter(t, storage, "operator", zap.NewNop(), nil)
	read := membershipRequest(router, http.MethodGet, ns.Name, "")
	require.Equal(t, http.StatusOK, read.Code)
	var before NamespaceItem
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &before))
	require.NotEmpty(t, before.PolicyResourceVersion)
	body := fmt.Sprintf(`{"users":["alice"],"policyResourceVersion":%q}`, before.PolicyResourceVersion)
	saved := membershipRequest(router, http.MethodPut, ns.Name, body)
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	var after NamespaceItem
	require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &after))
	require.NotEqual(t, before.PolicyResourceVersion, after.PolicyResourceVersion)
	require.Equal(t, []string{"alice"}, *after.Users)
	persisted := &corev1.ConfigMap{}
	require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(policy), persisted))
	require.Equal(t, persisted.ResourceVersion, after.PolicyResourceVersion)
	stale := membershipRequest(router, http.MethodPatch, ns.Name, body)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	compatible := membershipRequest(router, http.MethodPatch, ns.Name, `{"users":["bob"]}`)
	require.Equal(t, http.StatusOK, compatible.Code, compatible.Body.String())
	var item NamespaceItem
	require.NoError(t, json.Unmarshal(compatible.Body.Bytes(), &item))
	require.Equal(t, []string{"alice", "bob"}, *item.Users)
}

func TestHandlerListManagedNamespaces(t *testing.T) {
	bindings := []authorization.Binding{
		{Kind: rbacv1.UserKind, Name: "alice", Role: authorization.RoleEditor},
		{Kind: rbacv1.GroupKind, Name: "team", Role: authorization.RoleViewer},
	}
	for _, tc := range []struct {
		allowed []string
		want    []string
	}{
		{allowed: nil, want: []string{"alpha", "zeta"}},
		{allowed: []string{"zeta"}, want: []string{"zeta"}},
		{allowed: []string{"excluded"}, want: []string{}},
	} {
		t.Run(fmt.Sprint(tc.allowed), func(t *testing.T) {
			policy, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: "zeta", Bindings: bindings}).ConfigMap("capp-system", nil, nil)
			require.NoError(t, err)
			policy.ResourceVersion = "7"
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == "capp-system" {
						t.Error("discovery read protected namespace")
					}
					if len(tc.allowed) != 0 {
						if !strings.Contains(key.Name, "zeta") {
							t.Errorf("discovery read %q outside allowlist", key.Name)
						}
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}, managedNamespace("zeta"), policy, existingResourceQuota("zeta", "2", "4Gi"), managedNamespace("alpha"), managedNamespace("capp-system"), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged"}})
			core, logs := observer.New(zap.InfoLevel)
			router := membershipRouter(t, storage, "operator", zap.New(core), nil, tc.allowed...)
			w := membershipRequest(router, http.MethodGet, "", "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Empty(t, logs.FilterMessage("namespace read").All())
			entries := logs.FilterMessage("namespace discovery").All()
			require.Len(t, entries, 1)
			require.Equal(t, "succeeded", entries[0].ContextMap()["outcome"])
			require.Len(t, logs.FilterMessage("authorization decision").All(), 1)
			var result NamespaceListResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			names := make([]string, 0, len(result.Items))
			for _, item := range result.Items {
				names = append(names, item.Name)
				require.True(t, item.CanEdit)
				if item.Name == "zeta" {
					require.NotNil(t, item.Users)
					require.Equal(t, []string{"alice"}, *item.Users)
					require.NotNil(t, item.Bindings)
					require.ElementsMatch(t, bindings, *item.Bindings)
					require.Equal(t, policy.ResourceVersion, item.PolicyResourceVersion)
					require.NotNil(t, item.Quota)
					require.Equal(t, "2", item.Quota.CPU)
					require.Equal(t, "4Gi", item.Quota.Memory)
				}
			}
			require.Equal(t, tc.want, names)
			require.Equal(t, len(tc.want), result.Total)
			require.True(t, result.CanCreate)
		})
	}

	for _, tc := range []struct {
		name    string
		change  func(*corev1.ConfigMap)
		canEdit bool
	}{
		{name: "unowned policy", change: func(policy *corev1.ConfigMap) { policy.Labels = nil }},
		{name: "terminating policy", change: func(policy *corev1.ConfigMap) { policy.DeletionTimestamp = &metav1.Time{Time: time.Now()} }},
		{name: "namespace UID mismatch", change: func(policy *corev1.ConfigMap) { policy.Annotations[authorization.NamespaceUIDAnnotation] = "old" }},
		{name: "malformed owned policy remains repairable", change: func(policy *corev1.ConfigMap) { policy.Data["policy.json"] = "malformed" }, canEdit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, policy := membershipObjects(t, []authorization.Binding{{Kind: rbacv1.UserKind, Name: "untrusted", Role: authorization.RoleEditor}})
			tc.change(policy)
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{}, managedNamespace("alpha"), ns, policy, managedNamespace("zeta"), existingResourceQuota(ns.Name, "2", "4Gi"))
			router := membershipRouter(t, storage, "operator", zap.NewNop(), nil)
			w := membershipRequest(router, http.MethodGet, "", "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var result NamespaceListResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.Len(t, result.Items, 3)
			require.Equal(t, 3, result.Total)
			require.True(t, result.CanCreate)
			require.Equal(t, "alpha", result.Items[0].Name)
			require.True(t, result.Items[0].CanEdit)
			require.Equal(t, "zeta", result.Items[2].Name)
			require.True(t, result.Items[2].CanEdit)
			item := result.Items[1]
			require.Equal(t, ns.Name, item.Name)
			require.Equal(t, string(corev1.NamespaceActive), item.Status)
			require.Equal(t, "invalid", item.PolicyState)
			require.Equal(t, tc.canEdit, item.CanEdit)
			require.Nil(t, item.Users)
			require.Nil(t, item.Bindings)
			require.NotContains(t, w.Body.String(), "untrusted")
			require.NotNil(t, item.Quota)
			require.Equal(t, "2", item.Quota.CPU)
		})
	}

	for _, tc := range []struct {
		name          string
		quota         bool
		unownedPolicy bool
		err           error
		status        int
	}{
		{name: "policy unavailable", err: k8serrors.NewServiceUnavailable("private"), status: http.StatusServiceUnavailable},
		{name: "storage conflict", err: apierrors.NewConflict("storage conflict"), status: http.StatusConflict},
		{name: "quota unavailable", quota: true, err: k8serrors.NewServiceUnavailable("private"), status: http.StatusServiceUnavailable},
		{name: "quota unavailable for unowned policy", quota: true, unownedPolicy: true, err: k8serrors.NewServiceUnavailable("private"), status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, policy := membershipObjects(t, []authorization.Binding{})
			if tc.unownedPolicy {
				policy.Labels = nil
			}
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					_, quota := obj.(*corev1.ResourceQuota)
					if quota == tc.quota {
						return tc.err
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}, ns, policy)
			core, logs := observer.New(zap.InfoLevel)
			router := membershipRouter(t, storage, "operator", zap.New(core), nil)
			w := membershipRequest(router, http.MethodGet, "", "")
			require.Equal(t, tc.status, w.Code, w.Body.String())
			entries := logs.FilterMessage("namespace discovery").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, "failed", fields["outcome"])
			require.Equal(t, ns.Name, fields["namespace"])
			if tc.quota {
				require.Equal(t, string(policy.UID), fields["policyUID"])
				require.Equal(t, policy.ResourceVersion, fields["policyVersion"])
			}
			data, err := json.Marshal(fields)
			require.NoError(t, err)
			require.NotContains(t, string(data), "private")
			require.NotContains(t, w.Body.String(), "items")
		})
	}
}

func TestHandlerGetManagedNamespaceDenied(t *testing.T) {
	for _, scenario := range []string{"allowlist", "protected", "wrong target", "missing decision", "nonoperator before initial sync"} {
		t.Run(scenario, func(t *testing.T) {
			reads := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				reads++
				return nil
			}})
			user, name := "operator", "team-a"
			wantStatus := http.StatusForbidden
			var before gin.HandlerFunc
			switch scenario {
			case "protected":
				name = "capp-system"
			case "wrong target":
				before = func(c *gin.Context) {
					c.Params = append(gin.Params(nil), c.Params...)
					for i := range c.Params {
						if c.Params[i].Key == "namespace" {
							c.Params[i].Value = "other"
						}
					}
				}
			case "missing decision":
				before = func(c *gin.Context) { delete(c.Keys, "policyDecision") }
			case "nonoperator before initial sync":
				user = "reader"
				wantStatus = http.StatusServiceUnavailable
			}
			allowed := []string{}
			if scenario == "allowlist" {
				allowed = []string{"other"}
			}
			router := membershipRouter(t, storage, user, zap.NewNop(), before, allowed...)
			w := membershipRequest(router, http.MethodGet, name, "")
			require.Equal(t, wantStatus, w.Code, w.Body.String())
			require.Zero(t, reads)
		})
	}
}

func TestHandlerGetManagedNamespaceReadFailure(t *testing.T) {
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "private", fmt.Errorf("sensitive upstream error"))
	}})
	core, logs := observer.New(zap.InfoLevel)
	router := membershipRouter(t, storage, "operator", zap.New(core), nil)
	w := membershipRequest(router, http.MethodGet, "team-a", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "sensitive")
	entries := logs.FilterMessage("namespace read").All()
	require.Len(t, entries, 1)
	require.Equal(t, string(metav1.StatusReasonForbidden), entries[0].ContextMap()["failureReason"])
	require.NotContains(t, fmt.Sprint(entries), "sensitive")
}

func TestHandlerCreateManagedNamespaceResponse(t *testing.T) {
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{})
	router := membershipRouter(t, storage, "operator", zap.NewNop(), nil)
	w := membershipRequest(router, http.MethodPost, "", `{"name":"team-a","users":["alice"],"quota":{"cpu":"2"}}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var item NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	require.Equal(t, []string{"alice"}, *item.Users)
	require.Equal(t, []authorization.Binding{{Kind: rbacv1.UserKind, Name: "alice", Role: authorization.RoleEditor}}, *item.Bindings)
	require.NotEmpty(t, item.PolicyResourceVersion)
	require.Equal(t, "2", item.Quota.CPU)
	require.True(t, item.CanEdit)
}

func TestHandlerKubernetesBindings(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		for _, bindings := range []string{"[]", "null"} {
			t.Run(method+bindings, func(t *testing.T) {
				e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{managedNamespace("team-a")})
				path := "/namespaces/team-a"
				if method == http.MethodPost {
					path = "/namespaces"
				}
				body := fmt.Sprintf(`{"name":"team-a","bindings":%s}`, bindings)
				var status int
				switch method {
				case http.MethodPost:
					status = e.Post(path, strings.NewReader(body)).Code
				case http.MethodPut:
					status = e.Put(path, strings.NewReader(body)).Code
				case http.MethodPatch:
					status = e.Patch(path, strings.NewReader(body)).Code
				}
				require.Equal(t, http.StatusBadRequest, status)
			})
		}
	}
}

func TestHandlerGetManagedNamespaceObservation(t *testing.T) {
	ns, policy := membershipObjects(t, []authorization.Binding{{Kind: rbacv1.UserKind, Name: "reader", Role: authorization.RoleViewer}})
	ns.Labels[consts.ManagedNameSpaceLabelKey] = consts.ManagedLabelValue
	store, err := authorization.NewStore(fake.NewClientset(ns, policy), authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool { return store.Health().Synced }, 5*time.Second, 10*time.Millisecond)
	// Storage deliberately has no namespace or policy. Only quota reads are allowed.
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		require.IsType(t, &corev1.ResourceQuota{}, obj)
		return c.Get(ctx, key, obj, opts...)
	}}, existingResourceQuota(ns.Name, "1", ""))
	router := membershipRouterWithStore(t, storage, "reader", zap.NewNop(), nil, store)
	w := membershipRequest(router, http.MethodGet, ns.Name, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var item NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &item))
	require.False(t, item.CanEdit)
	require.Equal(t, policy.ResourceVersion, item.PolicyResourceVersion)
	require.Equal(t, []authorization.Binding{{Kind: rbacv1.UserKind, Name: "reader", Role: authorization.RoleViewer}}, *item.Bindings)
	require.Empty(t, *item.Users)
	require.Equal(t, "1", item.Quota.CPU)
}

func TestHandlerListAuthorizedNamespaces(t *testing.T) {
	objects := make([]runtime.Object, 0, 12)
	for _, name := range []string{"zeta", "alpha", "unmanaged", "pending", "recreated", "capp-system"} {
		ns, policy := membershipObjects(t, []authorization.Binding{})
		ns.Name = name
		policy.Name = "capp-assignment-" + name
		policy.Data["policy.json"] = fmt.Sprintf(`{"version":1,"namespace":%q,"bindings":[{"kind":"User","name":"reader","role":"viewer"},{"kind":"User","name":"editor","role":"editor"}]}`, name)
		switch name {
		case "unmanaged":
			ns.Labels = nil
		case "pending":
			delete(policy.Annotations, authorization.NamespaceUIDAnnotation)
		case "recreated":
			ns.UID = "replacement"
		}
		objects = append(objects, ns, policy)
	}
	observations := fake.NewClientset(objects...)
	store, err := authorization.NewStore(observations, authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool { return store.Health().Synced }, 5*time.Second, time.Millisecond)
	unsynced, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, user    string
		store         *authorization.Store
		allowed, want []string
		status        int
	}{
		{name: "viewer sorted eligible namespaces", user: "reader", store: store, want: []string{"alpha", "zeta"}, status: http.StatusOK},
		{name: "editor discovery", user: "editor", store: store, want: []string{"alpha", "zeta"}, status: http.StatusOK},
		{name: "allowlist filters reads and totals", user: "reader", store: store, allowed: []string{"zeta"}, want: []string{"zeta"}, status: http.StatusOK},
		{name: "no permitted namespaces", user: "reader", store: store, allowed: []string{"other"}, want: []string{}, status: http.StatusOK},
		{name: "no grants", user: "stranger", store: store, want: []string{}, status: http.StatusOK},
		{name: "incomplete observations", user: "reader", store: unsynced, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := []string{}
			var readsMu sync.Mutex
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.ResourceQuota); !ok {
						t.Errorf("discovery read %T, want ResourceQuota", obj)
					}
					if !slices.Contains(tc.want, key.Namespace) {
						t.Errorf("discovery read %q outside authorized namespaces", key.Namespace)
					}
					readsMu.Lock()
					reads = append(reads, key.Namespace)
					readsMu.Unlock()
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("ordinary discovery must not list live resources")
					return nil
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					t.Fatal("ordinary discovery must not write or review Kubernetes permissions")
					return nil
				},
			}, existingResourceQuota("alpha", "1", ""), existingResourceQuota("zeta", "2", ""))
			core, logs := observer.New(zap.InfoLevel)
			router := membershipRouterWithStore(t, storage, tc.user, zap.New(core), nil, tc.store, tc.allowed...)
			observations.ClearActions()
			w := membershipRequest(router, http.MethodGet, "", "")
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Empty(t, observations.Actions(), "discovery must use existing observations")
			if tc.status != http.StatusOK {
				require.Contains(t, w.Body.String(), "POLICY_UNAVAILABLE")
				require.Empty(t, reads)
				return
			}
			require.Empty(t, logs.FilterMessage("namespace read").All())
			entries := logs.FilterMessage("namespace discovery").All()
			require.Len(t, entries, 1)
			require.Equal(t, "succeeded", entries[0].ContextMap()["outcome"])
			require.Len(t, logs.FilterMessage("authorization decision").All(), 1)
			var result NamespaceListResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.False(t, result.CanCreate)
			require.Equal(t, len(tc.want), result.Total)
			require.NotNil(t, result.Items)
			names := make([]string, 0, len(result.Items))
			for _, item := range result.Items {
				names = append(names, item.Name)
				require.False(t, item.CanEdit)
				require.Equal(t, "1", item.PolicyResourceVersion)
				require.Equal(t, []string{"editor"}, *item.Users)
				require.Len(t, *item.Bindings, 2)
				require.NotNil(t, item.Quota)
			}
			require.Equal(t, tc.want, names)
			require.ElementsMatch(t, tc.want, reads)
		})
	}
}

func TestHandlerListAuthorizedNamespacesChanges(t *testing.T) {
	ns, policy := membershipObjects(t, []authorization.Binding{{Kind: rbacv1.UserKind, Name: "reader", Role: authorization.RoleViewer}})
	observations := fake.NewClientset(ns, policy)
	store, err := authorization.NewStore(observations, authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool { return store.Health().Synced }, 5*time.Second, time.Millisecond)
	reads := 0
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		require.IsType(t, &corev1.ResourceQuota{}, obj)
		reads++
		return c.Get(ctx, key, obj, opts...)
	}})
	router := membershipRouterWithStore(t, storage, "reader", zap.NewNop(), nil, store)
	check := func(want int) {
		t.Helper()
		reads = 0
		w := membershipRequest(router, http.MethodGet, "", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var result NamespaceListResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.Equal(t, want, result.Total)
		require.Len(t, result.Items, want)
		require.Equal(t, want, reads)
	}
	check(1)
	original := policy.Data["policy.json"]
	policy.Data["policy.json"] = `{"version":1,"namespace":"team-a","bindings":[]}`
	policy.ResourceVersion = "2"
	_, err = observations.CoreV1().ConfigMaps(policy.Namespace).Update(t.Context(), policy, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Lookup(ns.Name).PolicyResourceVersion == "2" }, 5*time.Second, time.Millisecond)
	check(0)
	policy.Data["policy.json"] = original
	policy.ResourceVersion = "3"
	_, err = observations.CoreV1().ConfigMaps(policy.Namespace).Update(t.Context(), policy, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Lookup(ns.Name).PolicyResourceVersion == "3" }, 5*time.Second, time.Millisecond)
	check(1)
	ns.UID = "replacement"
	_, err = observations.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Lookup(ns.Name).Reason == authorization.NamespaceUIDMismatch }, 5*time.Second, time.Millisecond)
	check(0)
}

func TestReadNamespaceItems(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Buffer one notification per concurrent read so workers can report readiness.
	started := make(chan struct{}, namespaceReadConcurrency)
	release := make(chan struct{})
	var active atomic.Int32
	done := make(chan struct{})
	var items []NamespaceItem
	var err error
	go func() {
		defer close(done)
		items, err = readNamespaceItems(ctx, namespaceReadConcurrency+2, func(ctx context.Context, i int) (NamespaceItem, error) {
			current := active.Add(1)
			defer active.Add(-1)
			if current > namespaceReadConcurrency {
				t.Errorf("concurrent reads = %d, want at most %d", current, namespaceReadConcurrency)
			}
			select {
			case started <- struct{}{}:
			case <-ctx.Done():
				return NamespaceItem{}, ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
				return NamespaceItem{}, ctx.Err()
			}
			return NamespaceItem{Name: fmt.Sprint(i)}, nil
		})
	}()
	for range namespaceReadConcurrency {
		select {
		case <-started:
		case <-ctx.Done():
			<-done
			t.Fatal("namespace reads did not overlap")
		}
	}
	close(release)
	<-done
	require.NoError(t, err)
	require.Len(t, items, namespaceReadConcurrency+2)
	for i, item := range items {
		require.Equal(t, fmt.Sprint(i), item.Name)
	}
}

func TestReadNamespaceItemsFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	failure := errors.New("storage unavailable")
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "policy-uid", ResourceVersion: "7"}}
	items, err := readNamespaceItems(ctx, 2, func(ctx context.Context, i int) (NamespaceItem, error) {
		if i == 0 {
			select {
			case <-started:
				return NamespaceItem{}, &namespaceReadError{namespace: "alpha", policy: policy, err: failure}
			case <-ctx.Done():
				return NamespaceItem{}, ctx.Err()
			}
		}
		close(started)
		<-ctx.Done()
		return NamespaceItem{}, ctx.Err()
	})
	require.ErrorIs(t, err, failure)
	require.Nil(t, items)
	var readError *namespaceReadError
	require.ErrorAs(t, err, &readError)
	require.Equal(t, "alpha", readError.namespace)
	require.Same(t, policy, readError.policy)
}

func TestReadNamespaceItemsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	items, err := readNamespaceItems(ctx, 2, func(context.Context, int) (NamespaceItem, error) {
		t.Error("canceled request started a storage read")
		return NamespaceItem{}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, items)
}

func TestReadNamespaceItemsCanceledDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	items, err := readNamespaceItems(ctx, 1, func(context.Context, int) (NamespaceItem, error) {
		// A completed read can succeed even as the request is canceled.
		cancel()
		return NamespaceItem{Name: "alpha"}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, items)
}

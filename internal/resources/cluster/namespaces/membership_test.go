package namespaces

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestHandlerUpdateManagedNamespace(t *testing.T) {
	editor := authorization.Binding{Kind: "User", Name: "alice", Role: "editor"}
	viewer := authorization.Binding{Kind: "User", Name: "alice", Role: "viewer"}
	group := authorization.Binding{Kind: "Group", Name: "developers", Role: "editor"}
	bob := authorization.Binding{Kind: "User", Name: "bob", Role: "editor"}
	for _, tc := range []struct {
		name, method, body string
		missing            bool
		want               []authorization.Binding
		writes             int
	}{
		{"users replace only direct editors", http.MethodPut, `{"users":["bob"]}`, false, []authorization.Binding{viewer, group, bob}, 1},
		{"bindings replace everything", http.MethodPut, `{"bindings":[{"kind":"User","name":"bob","role":"editor"}]}`, false, []authorization.Binding{bob}, 1},
		{"empty users preserve other grants", http.MethodPut, `{"users":[]}`, false, []authorization.Binding{viewer, group}, 1},
		{"empty bindings withdraw all grants", http.MethodPut, `{"bindings":[]}`, false, []authorization.Binding{}, 1},
		{"users add without replacement", http.MethodPatch, `{"users":["bob"]}`, false, []authorization.Binding{editor, viewer, group, bob}, 1},
		{"explicit bindings add viewer and group", http.MethodPatch, `{"bindings":[{"kind":"User","name":"bob","role":"viewer"},{"kind":"Group","name":"auditors","role":"viewer"}]}`, false, []authorization.Binding{editor, viewer, group, {Kind: "User", Name: "bob", Role: "viewer"}, {Kind: "Group", Name: "auditors", Role: "viewer"}}, 1},
		{"existing tuple is no-op", http.MethodPatch, `{"users":["alice"],"policyResourceVersion":"1"}`, false, []authorization.Binding{editor, viewer, group}, 0},
		{"omitted PUT membership", http.MethodPut, `{}`, false, []authorization.Binding{editor, viewer, group}, 0},
		{"omitted PATCH membership", http.MethodPatch, `{}`, false, []authorization.Binding{editor, viewer, group}, 0},
		{"PUT initializes missing policy", http.MethodPut, `{"users":["bob"]}`, true, []authorization.Binding{bob}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{editor, viewer, group})
			objects := []client.Object{ns}
			if !tc.missing {
				objects = append(objects, cm)
			}
			writes := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				},
			}, objects...)
			core, logs := observer.New(zap.InfoLevel)
			router := membershipRouter(t, storage, "operator", zap.New(core), nil)
			w := membershipRequest(router, tc.method, "team-a", tc.body)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, tc.writes, writes)
			persisted := &corev1.ConfigMap{}
			require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(cm), persisted))
			assignment, err := authorization.ParseAssignment(persisted, "capp-system", nil, nil)
			require.NoError(t, err)
			require.ElementsMatch(t, tc.want, assignment.Bindings())
			if !tc.missing {
				require.Equal(t, cm.UID, persisted.UID)
				require.Equal(t, cm.Annotations, persisted.Annotations)
				require.Equal(t, cm.Labels, persisted.Labels)
				require.Equal(t, cm.Finalizers, persisted.Finalizers)
			} else {
				require.Equal(t, string(ns.UID), persisted.Annotations[authorization.NamespaceUIDAnnotation])
			}
			audit := logs.FilterMessage("namespace membership mutation").All()
			require.Len(t, audit, 1)
			require.Equal(t, "operator", audit[0].ContextMap()["caller"])
			require.Equal(t, "succeeded", audit[0].ContextMap()["outcome"])
			require.Equal(t, persisted.ResourceVersion, audit[0].ContextMap()["policyVersion"])
		})
	}
}

func TestHandlerUpdateManagedNamespaceValidation(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
	}{
		{"mutually exclusive even empty", http.MethodPut, `{"users":[],"bindings":[]}`, http.StatusBadRequest},
		{"null users", http.MethodPut, `{"users":null}`, http.StatusBadRequest},
		{"null bindings", http.MethodPatch, `{"bindings":null}`, http.StatusBadRequest},
		{"duplicate users", http.MethodPut, `{"users":["alice","alice"]}`, http.StatusBadRequest},
		{"restricted user", http.MethodPut, `{"users":["system:admin"]}`, http.StatusBadRequest},
		{"restricted group", http.MethodPatch, `{"bindings":[{"kind":"Group","name":"system:authenticated","role":"editor"}]}`, http.StatusBadRequest},
		{"padded user", http.MethodPut, `{"users":["alice "]}`, http.StatusBadRequest},
		{"control in group", http.MethodPatch, `{"bindings":[{"kind":"Group","name":"dev\nops","role":"editor"}]}`, http.StatusBadRequest},
		{"blank user", http.MethodPut, `{"users":[" "]}`, http.StatusBadRequest},
		{"unsupported role", http.MethodPatch, `{"bindings":[{"kind":"User","name":"alice","role":"admin"}]}`, http.StatusBadRequest},
		{"unknown binding field", http.MethodPut, `{"bindings":[{"kind":"User","name":"alice","role":"editor","private-value":1}]}`, http.StatusBadRequest},
		{"duplicate field", http.MethodPut, `{"users":[],"users":[]}`, http.StatusBadRequest},
		{"trailing JSON", http.MethodPut, `{"users":[]} {}`, http.StatusBadRequest},
		{"null body", http.MethodPut, `null`, http.StatusBadRequest},
		{"PATCH quota", http.MethodPatch, `{"users":[],"quota":{"cpu":"2"}}`, http.StatusBadRequest},
		{"oversize request", http.MethodPut, strings.Repeat(" ", authorization.MaxPolicyBytes+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				calls++
				return errors.New("private storage error")
			}})
			router := membershipRouter(t, storage, "operator", zap.NewNop(), nil)
			w := membershipRequest(router, tc.method, "team-a", tc.body)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Zero(t, calls, "invalid input must not read storage")
			require.NotContains(t, w.Body.String(), "private")
		})
	}
}

func TestAdministrationPrepareMembership(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		patch      bool
		change     func(*corev1.Namespace, *corev1.ConfigMap)
		missing    bool
		status     int
	}{
		{name: "PATCH missing policy", body: `{"users":[]}`, patch: true, missing: true, status: http.StatusNotFound},
		{name: "supplied revision against missing policy", body: `{"users":[],"policyResourceVersion":"1"}`, missing: true, status: http.StatusConflict},
		{name: "stale revision even on no-op", body: `{"policyResourceVersion":"old"}`, patch: true, status: http.StatusConflict},
		{name: "unmanaged target", body: `{"users":[]}`, change: func(ns *corev1.Namespace, _ *corev1.ConfigMap) { ns.Labels = nil }, status: http.StatusConflict},
		{name: "terminating target", body: `{"users":[]}`, change: func(ns *corev1.Namespace, _ *corev1.ConfigMap) {
			ns.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			ns.Finalizers = []string{"keep"}
		}, status: http.StatusConflict},
		{name: "unowned policy", body: `{"bindings":[]}`, change: func(_ *corev1.Namespace, cm *corev1.ConfigMap) { cm.Labels = nil }, status: http.StatusConflict},
		{name: "terminating policy", body: `{"bindings":[]}`, change: func(_ *corev1.Namespace, cm *corev1.ConfigMap) {
			cm.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
		}, status: http.StatusConflict},
		{name: "namespace UID mismatch cannot be repaired", body: `{"bindings":[]}`, change: func(_ *corev1.Namespace, cm *corev1.ConfigMap) {
			cm.Annotations[authorization.NamespaceUIDAnnotation] = "old"
		}, status: http.StatusConflict},
		{name: "restricted policy rejects users replacement", body: `{"users":[]}`, change: func(_ *corev1.Namespace, cm *corev1.ConfigMap) {
			cm.Data["policy.json"] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"Group","name":"system:authenticated","role":"editor"}]}`
		}, status: http.StatusConflict},
		{name: "restricted policy permits full replacement", body: `{"bindings":[]}`, change: func(_ *corev1.Namespace, cm *corev1.ConfigMap) {
			cm.Data["policy.json"] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"Group","name":"system:authenticated","role":"editor"}]}`
		}, status: http.StatusOK},
		{name: "malformed policy rejects users replacement", body: `{"users":[]}`, change: invalidateMembershipPolicy, status: http.StatusConflict},
		{name: "omitted PATCH still rejects invalid policy", body: `{}`, patch: true, change: invalidateMembershipPolicy, status: http.StatusConflict},
		{name: "malformed policy rejects additive bindings", body: `{"bindings":[]}`, patch: true, change: invalidateMembershipPolicy, status: http.StatusConflict},
		{name: "full bindings repairs malformed policy", body: `{"bindings":[]}`, change: invalidateMembershipPolicy, status: http.StatusOK},
		{name: "omitted membership does not bootstrap", body: `{}`, missing: true, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			if tc.change != nil {
				tc.change(ns, cm)
			}
			objects := []client.Object{ns}
			if !tc.missing {
				objects = append(objects, cm)
			}
			writes := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				},
			}, objects...)
			method := http.MethodPut
			if tc.patch {
				method = http.MethodPatch
			}
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), method, "team-a", tc.body)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status != http.StatusOK || tc.missing {
				require.Zero(t, writes)
				return
			}
			persisted := &corev1.ConfigMap{}
			require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(cm), persisted))
			_, err := authorization.ParseAssignment(persisted, "capp-system", nil, nil)
			require.NoError(t, err)
			require.Equal(t, cm.Annotations, persisted.Annotations)
		})
	}
}

func TestHandlerUpdateManagedNamespaceDenials(t *testing.T) {
	for _, tc := range []struct {
		name, user, namespace string
		tamper                gin.HandlerFunc
	}{
		{name: "nonoperator", user: "editor", namespace: "team-a"},
		{name: "protected", user: "operator", namespace: "capp-system"},
		{name: "changed target", user: "operator", namespace: "team-a", tamper: func(c *gin.Context) { c.Params = append(c.Params[:1], gin.Param{Key: "namespace", Value: "other"}) }},
		{name: "missing decision", user: "operator", namespace: "team-a", tamper: func(c *gin.Context) { delete(c.Keys, "policyDecision") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				calls++
				return errors.New("unexpected read")
			}})
			router := membershipRouter(t, storage, tc.user, zap.NewNop(), tc.tamper)
			w := membershipRequest(router, http.MethodPut, tc.namespace, `{"users":["bob"]}`)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.Zero(t, calls)
		})
	}
}

func TestHandlerUpdateManagedNamespaceAudit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		readFailure bool
		err         error
		reason      metav1.StatusReason
		status      int
	}{
		{name: "forbidden write", err: k8serrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "private-upstream", errors.New("private-body")), reason: metav1.StatusReasonForbidden, status: http.StatusServiceUnavailable},
		{name: "timeout during preparation", readFailure: true, err: k8serrors.NewTimeoutError("private-timeout", 1), reason: metav1.StatusReasonTimeout, status: http.StatusServiceUnavailable},
		{name: "unrecognized upstream reason", err: &k8serrors.StatusError{ErrStatus: metav1.Status{Reason: "private-reason", Message: "private-body"}}, reason: metav1.StatusReasonUnknown, status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tc.readFailure {
						return tc.err
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					return tc.err
				},
			}, ns, cm)
			core, logs := observer.New(zap.InfoLevel)
			router := membershipRouter(t, storage, "operator", zap.New(core), nil)
			w := membershipRequest(router, http.MethodPut, "team-a", `{"users":["private-member"]}`)
			require.Equal(t, tc.status, w.Code)
			require.NotContains(t, w.Body.String(), "private")
			require.NotContains(t, w.Body.String(), "failureReason")
			for _, entry := range logs.All() {
				data, err := json.Marshal(entry.ContextMap())
				require.NoError(t, err)
				require.NotContains(t, string(data), "private")
			}
			audit := logs.FilterMessage("namespace membership mutation").All()
			require.Len(t, audit, 1)
			require.Equal(t, "failed", audit[0].ContextMap()["outcome"])
			require.Equal(t, string(tc.reason), audit[0].ContextMap()["failureReason"])
			if !tc.readFailure {
				require.Equal(t, string(cm.UID), audit[0].ContextMap()["policyUID"])
			}
		})
	}
}

func TestHandlerUpdateManagedNamespaceConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
	}{
		{name: "conditional update reports conflict without retry"},
		{name: "create-only bootstrap reports already exists without retry", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			objects := []client.Object{ns}
			if !tc.missing {
				objects = append(objects, cm)
			}
			writes := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					writes++
					require.True(t, tc.missing, "existing policies must use Update")
					require.Equal(t, client.ObjectKeyFromObject(cm), client.ObjectKeyFromObject(obj))
					require.Empty(t, obj.GetUID())
					require.Empty(t, obj.GetResourceVersion())
					return k8serrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
				},
				Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
					writes++
					require.False(t, tc.missing, "missing policies must use Create")
					require.Equal(t, client.ObjectKeyFromObject(cm), client.ObjectKeyFromObject(obj))
					require.Equal(t, cm.UID, obj.GetUID())
					require.Equal(t, cm.ResourceVersion, obj.GetResourceVersion())
					return k8serrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("private conflict"))
				},
			}, objects...)
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPut, ns.Name, `{"users":["bob"]}`)
			require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "private")
			require.Equal(t, 1, writes)
		})
	}
}

func membershipObjects(t *testing.T, bindings []authorization.Binding) (*corev1.Namespace, *corev1.ConfigMap) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: "namespace-uid", Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
	cm, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: ns.Name, Bindings: bindings}).ConfigMap("capp-system", nil, nil)
	require.NoError(t, err)
	cm.UID = "policy-uid"
	cm.ResourceVersion = "1"
	cm.Labels["other"] = "preserve"
	cm.Finalizers = []string{"example.org/keep"}
	cm.Annotations = map[string]string{authorization.NamespaceUIDAnnotation: string(ns.UID), "other": "preserve"}
	return ns, cm
}

func invalidateMembershipPolicy(_ *corev1.Namespace, cm *corev1.ConfigMap) {
	cm.Data = map[string]string{"invalid": "private-data"}
	cm.BinaryData = map[string][]byte{"invalid": {1}}
}

func membershipRouter(t *testing.T, storage client.Client, user string, logger *zap.Logger, before gin.HandlerFunc, allowedNamespaces ...string) *gin.Engine {
	t.Helper()
	store, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	return membershipRouterWithStore(t, storage, user, logger, before, store, allowedNamespaces...)
}

func membershipRouterWithStore(t *testing.T, storage client.Client, user string, logger *zap.Logger, before gin.HandlerFunc, store *authorization.Store, allowedNamespaces ...string) *gin.Engine {
	t.Helper()
	boundary, boundaryErr := cluster.New(nil, nil, zap.NewNop())
	require.NoError(t, boundaryErr)
	admin, err := NewAdministration(storage, "capp-system", nil, []string{"system:"})
	require.NoError(t, err)
	rules, err := policy.NewRoutes("")
	require.NoError(t, err)
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1", AllowedNamespaces: allowedNamespaces}}
	cc.SetHealthy(true)
	mgr := &testutil.MockClusterManager{
		GetFn:                func(string) (*cluster.ClusterClient, error) { return cc, nil },
		IsNamespaceAllowedFn: boundary.IsNamespaceAllowed,
	}
	identity := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
		return auth.ClusterCredential{ImpersonateUser: user}, nil
	}}
	r := gin.New()
	r.Use(middleware.Logging(logger), policy.New(identity, mgr, rules, map[string]policy.Target{"c1": {Store: store, OperatorUsers: []string{"operator"}}}, logger))
	if before != nil {
		r.Use(before)
	}
	h := NewWithAdministration(logger, map[string]*Administration{"c1": admin})
	group := r.Group("/api/v1/clusters/:cluster")
	h.RegisterRoutes(group)
	return r
}

func membershipRequest(r http.Handler, method, namespace, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	path := "/api/v1/clusters/c1/namespaces"
	if method != http.MethodPost && namespace != "" {
		path += "/" + namespace
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestAdministrationPrepareMembershipSizeLimit(t *testing.T) {
	ns, cm := membershipObjects(t, []authorization.Binding{{Kind: "User", Name: strings.Repeat("a", 300000), Role: "viewer"}})
	writes := 0
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		writes++
		return nil
	}}, ns, cm)
	body, err := json.Marshal(PatchNamespaceRequest{Users: new([]string{strings.Repeat("b", 300000)})})
	require.NoError(t, err)
	require.Less(t, len(body), authorization.MaxPolicyBytes)
	w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPatch, "team-a", string(body))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "512 KiB")
	require.Zero(t, writes)
}

func TestHandlerUpdateManagedNamespaceImmutablePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
	}{
		{"PUT changes data", http.MethodPut, `{"users":["alice"]}`, http.StatusConflict},
		{"PATCH adds binding", http.MethodPatch, `{"users":["alice"]}`, http.StatusConflict},
		{"PUT unchanged data", http.MethodPut, `{"bindings":[]}`, http.StatusOK},
		{"PATCH unchanged membership", http.MethodPatch, `{"users":[]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			immutable := true
			cm.Immutable = &immutable
			writes := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					writes++
					return errors.New("unexpected write")
				},
			}, ns, cm)
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), tc.method, ns.Name, tc.body)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Zero(t, writes)
		})
	}
}

func TestAdministrationSaveMembershipNamespaceRecreated(t *testing.T) {
	ns, cm := membershipObjects(t, []authorization.Binding{})
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{}, ns)
	admin, err := NewAdministration(storage, "capp-system", nil, nil)
	require.NoError(t, err)
	users := []string{"alice"}
	proposal, err := admin.prepareMembership(t.Context(), ns.Name, UpdateNamespaceRequest{Users: &users}, false)
	require.NoError(t, err)
	require.NoError(t, storage.Delete(t.Context(), ns))
	replacement := ns.DeepCopy()
	replacement.UID = "replacement-uid"
	replacement.ResourceVersion = ""
	require.NoError(t, storage.Create(t.Context(), replacement))
	require.NoError(t, admin.saveMembership(t.Context(), proposal))
	require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(cm), cm))
	require.Equal(t, string(ns.UID), cm.Annotations[authorization.NamespaceUIDAnnotation])
}

func TestAdministrationSaveMembershipUnassociatedPolicy(t *testing.T) {
	ns, cm := membershipObjects(t, []authorization.Binding{})
	delete(cm.Annotations, authorization.NamespaceUIDAnnotation)
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{}, ns, cm)
	admin, err := NewAdministration(storage, "capp-system", nil, nil)
	require.NoError(t, err)
	users := []string{"alice"}
	proposal, err := admin.prepareMembership(t.Context(), ns.Name, UpdateNamespaceRequest{Users: &users}, false)
	require.NoError(t, err)
	require.NoError(t, admin.saveMembership(t.Context(), proposal))
	require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(cm), cm))
	require.Empty(t, cm.Annotations[authorization.NamespaceUIDAnnotation])
}

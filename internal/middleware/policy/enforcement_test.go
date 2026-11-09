package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	requestpolicy "github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNew(t *testing.T) {
	store, observations := policyStore(t, nil)
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1"}}
	cc.SetHealthy(true)
	execution := testutil.FakeClient(t)
	tests := []struct {
		name, method, path, user        string
		groups                          []string
		allowedNamespaces               []string
		authErr                         error
		unhealthy, noStore, noExecution bool
		operator                        bool
		status                          int
		code, reason                    string
	}{
		{name: "explicitly allowed viewer", method: http.MethodGet, user: "alice", allowedNamespaces: []string{"team-a"}, status: http.StatusOK},
		{name: "grant outside allowlist", method: http.MethodGet, user: "alice", allowedNamespaces: []string{"team-b"}, status: http.StatusForbidden, code: apierrors.CodeForbidden, reason: apierrors.ReasonPolicyDenied},
		{name: "operator resource grant", method: http.MethodGet, user: "alice", operator: true, status: http.StatusOK},
		{name: "viewer read", method: http.MethodGet, user: "alice", status: http.StatusOK},
		{name: "group editor mutation", method: http.MethodDelete, user: "alice", groups: []string{"developers"}, status: http.StatusOK},
		{name: "viewer mutation", method: http.MethodDelete, user: "alice", status: http.StatusForbidden, code: apierrors.CodeForbidden, reason: "POLICY_DENIED"},
		{name: "unassigned read", method: http.MethodGet, user: "bob", status: http.StatusForbidden, code: apierrors.CodeForbidden, reason: "POLICY_DENIED"},
		{name: "unauthenticated", method: http.MethodGet, authErr: auth.ErrUnauthenticated, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized},
		{name: "expired identity", method: http.MethodGet, authErr: auth.ErrTokenExpired, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized},
		{name: "directory unavailable", method: http.MethodGet, authErr: auth.ErrDirectoryUnavailable, status: http.StatusServiceUnavailable, code: apierrors.CodeInternal},
		{name: "unexpected authentication failure", method: http.MethodGet, authErr: errors.New("private-auth-cause"), status: http.StatusInternalServerError, code: apierrors.CodeInternal},
		{name: "empty verified username", method: http.MethodGet, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized},
		{name: "unknown cluster", method: http.MethodGet, user: "alice", path: "/api/v1/clusters/other/namespaces/team-a/configmaps/item", status: http.StatusNotFound, code: apierrors.CodeClusterNotFound},
		{name: "unhealthy cluster", method: http.MethodGet, user: "alice", unhealthy: true, status: http.StatusServiceUnavailable, code: apierrors.CodeClusterUnhealthy},
		{name: "protected namespace", method: http.MethodGet, user: "alice", path: "/api/v1/clusters/c1/namespaces/capp-system/configmaps/item", status: http.StatusForbidden, code: apierrors.CodeForbidden, reason: "POLICY_DENIED"},
		{name: "invalid namespace", method: http.MethodGet, user: "alice", path: "/api/v1/clusters/c1/namespaces/Bad_Name/configmaps/item", status: http.StatusForbidden, code: apierrors.CodeForbidden, reason: "POLICY_DENIED"},
		{name: "empty namespace", method: http.MethodGet, user: "alice", path: "/api/v1/clusters/c1/namespaces//configmaps/item", status: http.StatusForbidden, code: apierrors.CodeForbidden, reason: "POLICY_DENIED"},
		{name: "wrong namespace", method: http.MethodGet, user: "alice", path: "/api/v1/clusters/c1/namespaces/team-b/configmaps/item", status: http.StatusForbidden, code: apierrors.CodeForbidden, reason: "POLICY_DENIED"},
		{name: "store missing", method: http.MethodGet, user: "alice", noStore: true, status: http.StatusServiceUnavailable, code: apierrors.CodeInternal, reason: "POLICY_UNAVAILABLE"},
		{name: "execution unavailable", method: http.MethodGet, user: "alice", noExecution: true, status: http.StatusServiceUnavailable, code: apierrors.CodeInternal, reason: "POLICY_UNAVAILABLE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc.SetHealthy(!tt.unhealthy)
			cc.Meta.AllowedNamespaces = tt.allowedNamespaces
			target := requestpolicy.Target{Store: store, ExecutionClient: execution}
			if tt.operator {
				target.OperatorUsers = []string{tt.user}
			}
			if tt.noStore {
				target.Store = nil
			}
			if tt.noExecution {
				target.ExecutionClient = nil
			}
			core, logs := observer.New(zap.InfoLevel)
			am := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
				return auth.ClusterCredential{ImpersonateUser: tt.user, ImpersonateGroups: tt.groups}, tt.authErr
			}}
			r := policyRouter(t, am, policyManager(t, cc), target, zap.New(core))
			called := false
			handler := func(c *gin.Context) {
				called = true
				_, admin := c.Get(string(middleware.AdminK8sClientKey))
				require.False(t, admin)
				require.True(t, requestpolicy.RequireExecution(c))
				c.Status(http.StatusOK)
			}
			// Deliberately registered directly on the engine, outside protected groups.
			r.Handle(tt.method, "/api/v1/clusters/:cluster/namespaces/:namespace/configmaps/:name", handler)
			path := tt.path
			if path == "" {
				path = "/api/v1/clusters/c1/namespaces/team-a/configmaps/item"
			}
			observations.ClearActions()
			w := policyRequest(r, tt.method, path)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			require.Equal(t, tt.status == http.StatusOK, called)
			require.Empty(t, observations.Actions(), "requests must use observed policy, not live policy reads")
			if tt.code != "" {
				requirePolicyError(t, w, tt.code, tt.reason)
			}
			audits := logs.FilterMessage("authorization decision").All()
			require.Len(t, audits, 1)
			fields := audits[0].ContextMap()
			require.Equal(t, tt.operator, fields["operator"])
			if called {
				require.Equal(t, "policy", fields["authority"])
			} else {
				require.Equal(t, "none", fields["authority"])
			}
			require.Equal(t, tt.user, fields["caller"])
			require.Equal(t, tt.method, fields["method"])
			require.Equal(t, "/api/v1/clusters/:cluster/namespaces/:namespace/configmaps/:name", fields["route"])
			if called {
				require.Equal(t, "allowed", fields["outcome"])
				require.Equal(t, "policy-uid", fields["policyUID"])
				require.Equal(t, "1", fields["policyVersion"])
			} else {
				require.Equal(t, "denied", fields["outcome"])
			}
			require.NotContains(t, w.Body.String(), "private-auth-cause")
			encodedLogs, err := json.Marshal(audits)
			require.NoError(t, err)
			require.NotContains(t, string(encodedLogs), "private-auth-cause")
		})
	}
}

func TestNewOperators(t *testing.T) {
	store, observations := policyStore(t, nil)
	unobserved := fake.NewClientset()
	unsynced, err := authorization.NewStore(unobserved, authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	for _, tt := range []struct {
		name, user, method, suffix, clusterName string
		groups, operatorUsers, operatorGroups   []string
		allowedNamespaces                       []string
		synced                                  bool
		status                                  int
	}{
		{name: "operator detail outside allowlist", user: "bob", operatorUsers: []string{"bob"}, allowedNamespaces: []string{"team-b"}, method: http.MethodGet, status: http.StatusForbidden},
		{name: "allowed operator without observations", user: "bob", operatorUsers: []string{"bob"}, allowedNamespaces: []string{"team-a"}, method: http.MethodPut, status: http.StatusOK},
		{name: "operator user without observations", user: "bob", operatorUsers: []string{"bob"}, method: http.MethodPut, status: http.StatusOK},
		{name: "operator group", user: "bob", groups: []string{"admins"}, operatorGroups: []string{"admins"}, method: http.MethodPatch, status: http.StatusOK},
		{name: "user OR group", user: "bob", operatorUsers: []string{"other"}, groups: []string{"admins"}, operatorGroups: []string{"admins"}, method: http.MethodDelete, status: http.StatusOK},
		{name: "create decision without body target", user: "bob", operatorUsers: []string{"bob"}, method: http.MethodPost, status: http.StatusOK},
		{name: "operator detail without observations", user: "bob", operatorUsers: []string{"bob"}, method: http.MethodGet, status: http.StatusOK},
		{name: "operator discovery without observations", user: "bob", operatorUsers: []string{"bob"}, method: http.MethodGet, suffix: "/", status: http.StatusOK},
		{name: "empty lists", user: "bob", method: http.MethodPut, status: http.StatusForbidden},
		{name: "case sensitive user", user: "Bob", operatorUsers: []string{"bob"}, method: http.MethodPut, status: http.StatusForbidden},
		{name: "case sensitive group", user: "bob", groups: []string{"Admins"}, operatorGroups: []string{"admins"}, method: http.MethodPut, status: http.StatusForbidden},
		{name: "editor cannot invite", user: "editor", synced: true, method: http.MethodPatch, status: http.StatusForbidden},
		{name: "operator configuration is cluster scoped", user: "bob", operatorUsers: []string{"bob"}, clusterName: "c2", method: http.MethodPut, status: http.StatusForbidden},
		{name: "protected target", user: "bob", operatorUsers: []string{"bob"}, method: http.MethodDelete, suffix: "/capp-system", status: http.StatusForbidden},
		{name: "invalid target", user: "bob", operatorUsers: []string{"bob"}, method: http.MethodPut, suffix: "/Bad_Name", status: http.StatusForbidden},
		{name: "operator has no resource grant", user: "bob", operatorUsers: []string{"bob"}, synced: true, method: http.MethodGet, suffix: "/team-a/configmaps/item", status: http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := requestpolicy.Target{Store: unsynced, OperatorUsers: tt.operatorUsers, OperatorGroups: tt.operatorGroups}
			if tt.synced {
				target.Store = store
			}
			am := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
				return auth.ClusterCredential{ImpersonateUser: tt.user, ImpersonateGroups: tt.groups}, nil
			}}
			mgr := policyManager(t, nil)
			get := mgr.GetFn
			lookups := 0
			mgr.GetFn = func(name string) (*cluster.ClusterClient, error) {
				lookups++
				cc, err := get(name)
				if cc != nil {
					cc.Meta.AllowedNamespaces = tt.allowedNamespaces
				}
				return cc, err
			}
			core, logs := observer.New(zap.InfoLevel)
			r := policyRouter(t, am, mgr, target, zap.New(core))
			called := false
			handler := func(c *gin.Context) {
				called = true
				operator, _, allowed := requestpolicy.RequireNamespaceAccess(c)
				require.True(t, allowed)
				require.True(t, operator)
				_, execution := c.Get(string(middleware.K8sClientKey))
				_, admin := c.Get(string(middleware.AdminK8sClientKey))
				require.False(t, execution)
				require.False(t, admin)
				c.Status(http.StatusOK)
			}
			const route = "/api/v1/clusters/:cluster/namespaces"
			r.Handle(tt.method, route, handler)
			r.Handle(tt.method, route+"/:namespace", handler)
			r.Handle(tt.method, route+"/:namespace/configmaps/:name", handler)
			suffix := tt.suffix
			if suffix == "" && tt.method != http.MethodPost {
				suffix = "/team-a"
			}
			suffix = strings.TrimSuffix(suffix, "/")
			observations.ClearActions()
			clusterName := tt.clusterName
			if clusterName == "" {
				clusterName = "c1"
			}
			w := policyRequest(r, tt.method, "/api/v1/clusters/"+clusterName+"/namespaces"+suffix)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			require.Equal(t, tt.status == http.StatusOK, called)
			require.Empty(t, observations.Actions())
			require.Empty(t, unobserved.Actions())
			audits := logs.FilterMessage("authorization decision").All()
			require.Len(t, audits, 1)
			fields := audits[0].ContextMap()
			switch {
			case !called:
				require.Equal(t, "none", fields["authority"])
			case tt.method == http.MethodGet && suffix == "":
				require.Equal(t, "deferred", fields["authority"])
				require.Equal(t, true, fields["operator"])
			default:
				require.Equal(t, "operator", fields["authority"])
				require.Equal(t, true, fields["operator"])
			}
			if len(tt.operatorUsers) == 0 && len(tt.operatorGroups) == 0 {
				require.Zero(t, lookups, "nonoperators must be denied before cluster preparation")
			}
		})
	}
}

func TestNewPublicAndUnclassified(t *testing.T) {
	for _, tt := range []struct {
		name, method, path      string
		status                  int
		authCalls, handlerCalls int
	}{
		{"public", http.MethodGet, "/healthz", http.StatusOK, 0, 1},
		{"preflight", http.MethodOptions, "/new-api", http.StatusNoContent, 0, 0},
		{"unclassified matched route", http.MethodGet, "/new-api", http.StatusForbidden, 1, 0},
		{"wrong public method", http.MethodPost, "/healthz", http.StatusForbidden, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var authCalls, handlerCalls int
			am := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
				authCalls++
				return auth.ClusterCredential{ImpersonateUser: "alice"}, nil
			}}
			mgr := &testutil.MockClusterManager{GetFn: func(string) (*cluster.ClusterClient, error) {
				t.Error("unexpected cluster lookup")
				return nil, cluster.ErrClusterNotFound
			}}
			r := policyRouter(t, am, mgr, requestpolicy.Target{}, zap.NewNop())
			r.Handle(tt.method, tt.path, func(c *gin.Context) { handlerCalls++; c.Status(http.StatusOK) })
			w := policyRequest(r, tt.method, tt.path)
			require.Equal(t, tt.status, w.Code)
			require.Equal(t, tt.authCalls, authCalls)
			require.Equal(t, tt.handlerCalls, handlerCalls)
		})
	}
}

func TestNewUnmatchedRoutes(t *testing.T) {
	am := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
		t.Error("unmatched route authenticated")
		return auth.ClusterCredential{}, auth.ErrUnauthenticated
	}}
	r := policyRouter(t, am, policyManager(t, nil), requestpolicy.Target{}, zap.NewNop())
	r.HandleMethodNotAllowed = true
	unexpected := func(*gin.Context) { t.Error("unmatched route reached a handler") }
	r.NoRoute(unexpected)
	r.NoMethod(unexpected)
	r.GET("/healthz", unexpected)
	for _, tt := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
	} {
		w := policyRequest(r, tt.method, tt.path)
		require.Equal(t, tt.status, w.Code)
	}
}

func TestNewObservation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*corev1.ConfigMap, *corev1.Namespace)
		user   string
		status int
		reason string
	}{
		{"invalid policy", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Data["policy.json"] = "private-invalid-payload" }, "alice", http.StatusForbidden, "POLICY_DENIED"},
		{"namespace replaced", func(_ *corev1.ConfigMap, ns *corev1.Namespace) { ns.UID = "replacement" }, "alice", http.StatusForbidden, "POLICY_DENIED"},
		{"pending assigned user", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Annotations = nil }, "alice", http.StatusServiceUnavailable, "POLICY_UNAVAILABLE"},
		{"pending unassigned user", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Annotations = nil }, "bob", http.StatusForbidden, "POLICY_DENIED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := policyStore(t, tt.change)
			r := policyRouter(t, policyIdentity(tt.user), policyManager(t, nil), requestpolicy.Target{Store: store}, zap.NewNop())
			r.GET("/api/v1/clusters/:cluster/namespaces/:namespace/configmaps", func(*gin.Context) { t.Error("denied handler ran") })
			w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/namespaces/team-a/configmaps")
			require.Equal(t, tt.status, w.Code)
			code := apierrors.CodeForbidden
			if tt.status == http.StatusServiceUnavailable {
				code = apierrors.CodeInternal
			}
			requirePolicyError(t, w, code, tt.reason)
			require.NotContains(t, w.Body.String(), "private-invalid-payload")
		})
	}
	t.Run("unsynchronized observations", func(t *testing.T) {
		store, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
		require.NoError(t, err)
		r := policyRouter(t, policyIdentity("alice"), policyManager(t, nil), requestpolicy.Target{Store: store}, zap.NewNop())
		r.GET("/api/v1/clusters/:cluster/namespaces/:namespace/configmaps", func(*gin.Context) { t.Error("denied handler ran") })
		for _, namespace := range []string{"team-a", "capp-system"} {
			w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/namespaces/"+namespace+"/configmaps")
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			require.JSONEq(t, `{"error":{"code":"INTERNAL_ERROR","message":"authorization policy is unavailable","status":503,"details":{"reason":"POLICY_UNAVAILABLE"}}}`, w.Body.String())
		}
	})
}

func TestNewExecutionNamespace(t *testing.T) {
	store, _ := policyStore(t, nil)
	execution := clientfake.NewClientBuilder().WithScheme(testutil.TestScheme(t)).WithRESTMapper(policyExecutionMapper()).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "team-a"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "team-b"}},
	).Build()
	r := policyRouter(t, policyIdentity("alice"), policyManager(t, nil), requestpolicy.Target{Store: store, ExecutionClient: execution}, zap.NewNop())
	r.GET("/api/v1/clusters/:cluster/namespaces/:namespace/secrets", func(c *gin.Context) {
		scoped, ok := c.MustGet(string(middleware.K8sClientKey)).(client.Client)
		require.True(t, ok)
		var secrets corev1.SecretList
		// Deliberately omit InNamespace to exercise the middleware's boundary.
		require.NoError(t, scoped.List(c.Request.Context(), &secrets))
		require.Len(t, secrets.Items, 1)
		require.Equal(t, "team-a", secrets.Items[0].Namespace)

		err := scoped.Get(c.Request.Context(), client.ObjectKey{Namespace: "team-b", Name: "credentials"}, &corev1.Secret{})
		require.ErrorContains(t, err, "does not match")
		c.Status(http.StatusOK)
	})
	w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/namespaces/team-a/secrets")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func policyStore(t *testing.T, change func(*corev1.ConfigMap, *corev1.Namespace)) (*authorization.Store, *fake.Clientset) {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "capp-assignment-team-a", Namespace: "capp-system", UID: "policy-uid", ResourceVersion: "1",
		Labels:      map[string]string{"rcs.dana.io/authorization": "backend-policy"},
		Annotations: map[string]string{authorization.NamespaceUIDAnnotation: "namespace-uid"},
	}, Data: map[string]string{"policy.json": `{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"alice","role":"viewer"},{"kind":"User","name":"editor","role":"editor"},{"kind":"Group","name":"developers","role":"editor"}]}`}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: "namespace-uid", Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}}
	if change != nil {
		change(cm, ns)
	}
	observations := fake.NewClientset(cm, ns)
	store, err := authorization.NewStore(observations, authorization.StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool { return store.Health().Synced }, 5*time.Second, time.Millisecond)
	return store, observations
}

func policyRouter(t *testing.T, am auth.AuthManager, mgr cluster.ClusterManager, target requestpolicy.Target, logger *zap.Logger) *gin.Engine {
	t.Helper()
	rules, err := requestpolicy.NewRoutes("")
	require.NoError(t, err)
	r := gin.New()
	r.Use(middleware.Logging(logger), middleware.Recovery(logger), middleware.CORS([]string{"*"}),
		requestpolicy.New(am, mgr, rules, map[string]requestpolicy.Target{"c1": target}, logger))
	return r
}

func policyIdentity(user string) *testutil.MockAuthManager {
	return &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
		return auth.ClusterCredential{ImpersonateUser: user}, nil
	}}
}

func policyManager(t *testing.T, cc *cluster.ClusterClient) *testutil.MockClusterManager {
	t.Helper()
	boundary, boundaryErr := cluster.New(nil, nil, zap.NewNop())
	require.NoError(t, boundaryErr)
	if cc == nil {
		cc = &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1"}}
		cc.SetHealthy(true)
	}
	return &testutil.MockClusterManager{
		GetFn: func(name string) (*cluster.ClusterClient, error) {
			if name != cc.Meta.Name {
				return nil, cluster.ErrClusterNotFound
			}
			return cc, nil
		},
		ClientForFn: func(*cluster.ClusterClient, auth.ClusterCredential) (client.Client, error) {
			t.Error("Kubernetes-mode client construction reached")
			return nil, errors.New("Kubernetes-mode client")
		},
		IsNamespaceAllowedFn: boundary.IsNamespaceAllowed,
	}
}

func policyRequest(r http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func requirePolicyError(t *testing.T, w *httptest.ResponseRecorder, code, reason string) {
	t.Helper()
	var response struct {
		Error apierrors.APIError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, code, response.Error.Code)
	if reason == apierrors.ReasonPolicyDenied {
		require.JSONEq(t, `{"error":{"code":"FORBIDDEN","message":"operation is not permitted","status":403,"details":{"reason":"POLICY_DENIED"}}}`, w.Body.String())
	}
	if reason != "" {
		require.Equal(t, reason, response.Error.Details["reason"])
	}
}

// Fake clients need explicit REST mappings for the namespace wrapper's scope checks.
func policyExecutionMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, cappv1alpha1.GroupVersion})
	for _, kind := range []string{"Secret", "SecretList", "ConfigMapList"} {
		mapper.Add(corev1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	for _, kind := range []string{"Capp", "CappList", "CappRevision", "CappRevisionList"} {
		mapper.Add(cappv1alpha1.GroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	return mapper
}

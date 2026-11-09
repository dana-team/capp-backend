package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	requestpolicy "github.com/dana-team/capp-backend/internal/middleware/policy"
	nshandler "github.com/dana-team/capp-backend/internal/resources/cluster/namespaces"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/benchmarks"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBackendPolicyExecutionAndRevocation(t *testing.T) {
	store, observations, policy := policyStore(t, nil)
	var reads atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer system-token" || r.Header.Get(transport.ImpersonateUserHeader) != "" || r.Header.Get(transport.ImpersonateGroupHeader) != "" {
			t.Error("execution must use the backend credential without caller impersonation")
		}
		w.Header().Set("Content-Type", runtime.ContentTypeJSON)
		var body any
		switch r.URL.Path {
		case "/api":
			body = metav1.APIVersions{Versions: []string{"v1"}}
		case "/apis":
			body = metav1.APIGroupList{}
		case "/api/v1":
			body = metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true}}}
		case "/api/v1/namespaces/team-a/configmaps":
			reads.Add(1)
			if r.URL.Query().Get("labelSelector") != consts.ManagedLabelKey+"="+consts.ManagedLabelValue {
				t.Error("managed resource selector missing")
			}
			body = corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, Items: []corev1.ConfigMap{}}
		default:
			t.Errorf("unexpected execution path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1"}, Scheme: testutil.TestScheme(t), RestConfig: &rest.Config{Host: upstream.URL, BearerToken: "system-token"}}
	cc.SetHealthy(true)
	clients, err := cluster.NewSystemClients(cc)
	require.NoError(t, err)
	r := policyRouter(t, policyIdentity("alice"), policyManager(t, cc), requestpolicy.Target{Store: store, ExecutionClient: clients.Resources}, zap.NewNop())
	configmaps.New().RegisterRoutes(r.Group("/api/v1/clusters/:cluster"))
	path := "/api/v1/clusters/c1/namespaces/team-a/configmaps"
	w := policyRequest(r, http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.EqualValues(t, 1, reads.Load())
	policy.Data["policy.json"] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"bob","role":"viewer"}]}`
	policy.ResourceVersion = "2"
	_, err = observations.CoreV1().ConfigMaps(policy.Namespace).Update(t.Context(), policy, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Lookup("team-a").PolicyResourceVersion == "2" }, 5*time.Second, time.Millisecond)
	w = policyRequest(r, http.MethodGet, path, "")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.EqualValues(t, 1, reads.Load(), "revoked request must perform no resource reads")
}

func TestBackendPolicyDeniedAPIs(t *testing.T) {
	var calls atomic.Int64
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer prometheus.Close()
	mgr := &testutil.MockClusterManager{GetFn: func(string) (*cluster.ClusterClient, error) {
		t.Error("denied API reached target resolution")
		return nil, cluster.ErrClusterNotFound
	}}
	r := policyRouter(t, policyIdentity("editor"), mgr, requestpolicy.Target{ExecutionClient: &clientThatMustNotRun{t: t}}, zap.NewNop())
	group := r.Group("/api/v1/clusters/:cluster")
	nshandler.New(zap.NewNop()).RegisterRoutes(group)
	benchmarks.New(config.BenchmarkConfig{VMURLs: []string{prometheus.URL}}, zap.NewNop()).RegisterRoutes(group)
	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, "/namespaces"},
		{http.MethodPut, "/namespaces/team-a"},
		{http.MethodPatch, "/namespaces/team-a"},
		{http.MethodDelete, "/namespaces/team-a"},
		{http.MethodGet, "/namespaces/team-a/capps/app/benchmark"},
		{http.MethodPost, "/namespaces/team-a/capps/app/benchmark"},
		{http.MethodGet, "/namespaces/team-a/capps/app/metrics/dashboards"},
		{http.MethodGet, "/namespaces/team-a/capps/app/metrics/query?query=up"},
		{http.MethodGet, "/namespaces/team-a/capps/app/metrics/query_range?query=up&start=1&end=2&step=1"},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			w := policyRequest(r, tt.method, "/api/v1/clusters/c1"+tt.path, "{}")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			requirePolicyError(t, w, apierrors.CodeForbidden, "POLICY_DENIED")
		})
	}
	require.Zero(t, calls.Load(), "denied API must not reach Prometheus")
}

func TestBackendPolicyTargetChanged(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	mgr := policyManager(t, nil)
	core, logs := observer.New(zap.InfoLevel)
	r := policyRouter(t, policyIdentity("editor"), mgr, requestpolicy.Target{Store: store, ExecutionClient: &clientThatMustNotRun{t: t}}, zap.New(core))
	r.Use(func(c *gin.Context) {
		for i := range c.Params {
			if c.Params[i].Key == "namespace" {
				c.Params[i].Value = "other"
			}
		}
		c.Next()
	})
	configmaps.New().RegisterRoutes(r.Group("/api/v1/clusters/:cluster"))
	w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/namespaces/team-a/configmaps", "")
	require.Equal(t, http.StatusForbidden, w.Code)
	requirePolicyError(t, w, apierrors.CodeForbidden, "POLICY_DENIED")
	audits := logs.FilterMessage("authorization decision").All()
	require.Len(t, audits, 1)
	require.Equal(t, "denied", audits[0].ContextMap()["outcome"])
	require.Equal(t, "none", audits[0].ContextMap()["authority"])
}

func TestBackendPolicyExecutionForbidden(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	execution := clientfake.NewClientBuilder().WithScheme(testutil.TestScheme(t)).WithRESTMapper(policyExecutionMapper()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return k8serrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("execution credential denied"))
		},
	}).Build()
	core, logs := observer.New(zap.InfoLevel)
	r := policyRouter(t, policyIdentity("alice"), policyManager(t, nil), requestpolicy.Target{Store: store, ExecutionClient: execution}, zap.New(core))
	configmaps.New().RegisterRoutes(r.Group("/api/v1/clusters/:cluster"))
	w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/namespaces/team-a/configmaps", "")
	require.Equal(t, http.StatusForbidden, w.Code)
	requirePolicyError(t, w, apierrors.CodeForbidden, "")
	audits := logs.FilterMessage("authorization decision").All()
	require.Len(t, audits, 1)
	require.Equal(t, "allowed", audits[0].ContextMap()["outcome"])
}

func policyStore(t *testing.T, change func(*corev1.ConfigMap, *corev1.Namespace)) (*authorization.Store, *fake.Clientset, *corev1.ConfigMap) {
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
	return store, observations, cm
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

func policyRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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

// Any unoverridden operation panics through the nil embedded client and fails
// the expected forbidden response, including unexpected writes.
type clientThatMustNotRun struct {
	client.Client
	t *testing.T
}

func (c *clientThatMustNotRun) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	c.t.Error("denied resource read")
	return errors.New("denied read")
}
func (c *clientThatMustNotRun) List(context.Context, client.ObjectList, ...client.ListOption) error {
	c.t.Error("denied resource list")
	return errors.New("denied list")
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

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/client-go/util/cert"
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
	newCappsHandler(t, false, nil, nil, config.ObservabilityConfig{
		Enabled:  true,
		QueryURL: prometheus.URL,
		Metrics:  []config.MetricsQuery{{Name: "cpu", Type: "gauge", Query: "up{$selector}", NamespaceLabel: "namespace", ObjectLabel: "capp"}},
	}).RegisterRoutes(group)
	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, "/namespaces"},
		{http.MethodPut, "/namespaces/team-a"},
		{http.MethodPatch, "/namespaces/team-a"},
		{http.MethodDelete, "/namespaces/team-a"},
		{http.MethodGet, "/namespaces/team-a/capps/app/metrics"},
		{http.MethodGet, "/namespaces/team-a/capps/app/metrics/cpu"},
		{http.MethodGet, "/namespaces/team-a/capps/app/metrics/cpu?start=1&end=2&step=1"},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			w := policyRequest(r, tt.method, "/api/v1/clusters/c1"+tt.path, "{}")
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			requirePolicyError(t, w, apierrors.CodeForbidden, apierrors.ReasonPolicyDenied)
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
	requirePolicyError(t, w, apierrors.CodeForbidden, apierrors.ReasonPolicyDenied)
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
		requestpolicy.New(am, config.AuthModePassthrough, mgr, rules, map[string]requestpolicy.Target{"c1": target}, logger))
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
	req.Header.Set("Authorization", "Bearer caller-token")
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

// policyProvider uses real authentication transports while allowing each test
// to supply only the resource responses relevant to its route.
type policyProvider struct {
	mode    string
	auth    auth.AuthManager
	manager *testutil.MockClusterManager
	clients map[string]*cluster.SystemClients
	reviews map[string]*policyReview
}

type policyReview struct {
	user   atomic.Pointer[authv1.UserInfo]
	status atomic.Int64
	calls  atomic.Int64
	before func(context.Context)
}

func newPolicyProvider(t *testing.T, mode string, users map[string]authv1.UserInfo, resource http.HandlerFunc) *policyProvider {
	t.Helper()
	p := &policyProvider{mode: mode, clients: make(map[string]*cluster.SystemClients), reviews: make(map[string]*policyReview)}
	clusters := make(map[string]*cluster.ClusterClient)
	cfg := &config.Config{Auth: config.AuthConfig{Mode: mode}, Authorization: config.AuthorizationConfig{Mode: config.AuthorizationModeBackendPolicy}}
	dependencies := auth.Dependencies{TargetReviewers: make(map[string]auth.TokenReviewer)}
	for name, user := range users {
		review := &policyReview{}
		review.user.Store(&user)
		p.reviews[name] = review
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+name+"-backend-token" {
				t.Error("target request did not use its backend credential")
			}
			for key := range r.Header {
				if strings.HasPrefix(key, "Impersonate-") {
					t.Errorf("target received impersonation header %s", key)
				}
			}
			if r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews" {
				if mode != config.AuthModePassthrough {
					t.Error("OpenShift token was reviewed at an execution target")
				}
				review.serve(t, w, r)
				return
			}
			if resource == nil {
				t.Errorf("unexpected resource call %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			resource(w, r)
		}))
		t.Cleanup(upstream.Close)
		cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: name}, Scheme: testutil.TestScheme(t), RestConfig: &rest.Config{
			Host: upstream.URL, BearerToken: name + "-backend-token", ContentConfig: rest.ContentConfig{ContentType: runtime.ContentTypeJSON, AcceptContentTypes: runtime.ContentTypeJSON}, TLSClientConfig: rest.TLSClientConfig{
				CAData: pem.EncodeToMemory(&pem.Block{Type: cert.CertificateBlockType, Bytes: upstream.Certificate().Raw}),
			}, Impersonate: rest.ImpersonationConfig{UserName: "must-not-impersonate"},
		}}
		cc.SetHealthy(true)
		clusters[name] = cc
		clients, err := cluster.NewSystemClients(cc)
		if err != nil {
			t.Fatal(err)
		}
		p.clients[name] = clients
		dependencies.TargetReviewers[name] = auth.TokenReviewer{Client: clients.Kubernetes.AuthenticationV1(), Authority: upstream.URL}
		cfg.Clusters = append(cfg.Clusters, config.ClusterConfig{Name: name})
	}
	if mode == config.AuthModeOpenShift {
		homeReview := p.reviews["c1"]
		home := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/oauth-authorization-server" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Authorization") != "Bearer home-backend-token" {
				t.Error("home review did not use its backend credential")
			}
			homeReview.serve(t, w, r)
		}))
		t.Cleanup(home.Close)
		ca := pem.EncodeToMemory(&pem.Block{Type: cert.CertificateBlockType, Bytes: home.Certificate().Raw})
		dependencies.HomeConfig = &rest.Config{Host: home.URL, BearerToken: "home-backend-token", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}
		cfg.Auth.OpenShift.APIServer, cfg.Auth.OpenShift.CACert = home.URL, base64.StdEncoding.EncodeToString(ca)
	}
	var err error
	p.auth, err = auth.New(cfg, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	p.manager = policyManager(t, nil)
	p.manager.GetFn = func(name string) (*cluster.ClusterClient, error) {
		if cc, ok := clusters[name]; ok {
			return cc, nil
		}
		return nil, cluster.ErrClusterNotFound
	}
	p.manager.ListFn = func() []cluster.ClusterMeta {
		items := make([]cluster.ClusterMeta, 0, len(clusters))
		for _, cc := range clusters {
			meta := cc.Meta
			meta.Healthy = cc.IsHealthy()
			items = append(items, meta)
		}
		return items
	}
	return p
}

func (p *policyProvider) router(t *testing.T, targets map[string]requestpolicy.Target) *gin.Engine {
	t.Helper()
	for name, target := range targets {
		target.ExecutionClient = p.clients[name].Resources
		targets[name] = target
	}
	rules, err := requestpolicy.NewRoutes("")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(requestpolicy.New(p.auth, p.mode, p.manager, rules, targets, zap.NewNop()))
	router.GET("/api/v1/clusters", listClusters(p.manager))
	router.GET("/api/v1/clusters/:cluster", getCluster(p.manager))
	return router
}

func (p *policyReview) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	p.calls.Add(1)
	var review authv1.TokenReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		t.Error(err)
		return
	}
	if review.Spec.Token != "caller-token" || len(review.Spec.Audiences) != 0 || r.Method != http.MethodPost {
		t.Error("review must carry only the caller token with unset audiences")
	}
	if p.before != nil {
		p.before(r.Context())
	}
	w.Header().Set("Content-Type", runtime.ContentTypeJSON)
	if status := p.status.Load(); status != 0 {
		w.WriteHeader(int(status))
		return
	}
	response := authv1.TokenReview{TypeMeta: metav1.TypeMeta{APIVersion: authv1.SchemeGroupVersion.String(), Kind: "TokenReview"}}
	if user := p.user.Load(); user != nil {
		response.Status.Authenticated, response.Status.User = true, *user
	}
	if err := json.NewEncoder(w).Encode(response); err != nil && r.Context().Err() == nil {
		t.Error(err)
	}
}

func TestBackendPolicyProviderRoutes(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	for _, mode := range []string{config.AuthModeOpenShift, config.AuthModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name, method, path, body string
				user                     authv1.UserInfo
				status, effects          int
			}{
				{name: "viewer read", method: http.MethodGet, path: "/namespaces/team-a/configmaps", user: authv1.UserInfo{Username: "alice"}, status: http.StatusOK, effects: 1},
				{name: "group editor delete", method: http.MethodDelete, path: "/namespaces/team-a/configmaps/item", user: authv1.UserInfo{Username: "group-user", Groups: []string{"developers"}}, status: http.StatusNoContent, effects: 3},
				{name: "viewer delete denied", method: http.MethodDelete, path: "/namespaces/team-a/configmaps/item", user: authv1.UserInfo{Username: "alice"}, status: http.StatusForbidden},
				{name: "operator has no resource grant", method: http.MethodGet, path: "/namespaces/team-a/configmaps", user: authv1.UserInfo{Username: "operator", Groups: []string{"operators"}}, status: http.StatusForbidden},
				{name: "group aggregate", method: http.MethodGet, path: "/configmaps", user: authv1.UserInfo{Username: "group-user", Groups: []string{"developers"}}, status: http.StatusOK, effects: 1},
				{name: "unassigned aggregate", method: http.MethodGet, path: "/configmaps", user: authv1.UserInfo{Username: "bob"}, status: http.StatusOK},
				{name: "operator creates namespace", method: http.MethodPost, path: "/namespaces", body: `{"name":"new"}`, user: authv1.UserInfo{Username: "operator", Groups: []string{"operators"}}, status: http.StatusCreated, effects: 4},
				{name: "editor cannot administer", method: http.MethodPost, path: "/namespaces", body: `{"name":"new"}`, user: authv1.UserInfo{Username: "editor"}, status: http.StatusForbidden},
				{name: "metrics denied", method: http.MethodGet, path: "/namespaces/team-a/capps/app/metrics", user: authv1.UserInfo{Username: "editor"}, status: http.StatusForbidden},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var effects atomic.Int64
					p := newPolicyProvider(t, mode, map[string]authv1.UserInfo{"c1": tc.user}, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", runtime.ContentTypeJSON)
						var body any
						switch r.URL.Path {
						case "/api":
							body = metav1.APIVersions{Versions: []string{corev1.SchemeGroupVersion.Version}}
						case "/apis":
							body = metav1.APIGroupList{}
						case "/api/v1":
							body = metav1.APIResourceList{GroupVersion: corev1.SchemeGroupVersion.String(), APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true}, {Name: "namespaces", Kind: "Namespace"}}}
						case "/apis/rcs.dana.io/v1alpha1":
							body = metav1.APIResourceList{GroupVersion: cappv1alpha1.GroupVersion.String(), APIResources: []metav1.APIResource{{Name: consts.CappResource, Kind: "Capp", Namespaced: true}}}
						case "/apis/rcs.dana.io/v1alpha1/namespaces/team-a/capps":
							effects.Add(1)
							body = cappv1alpha1.CappList{TypeMeta: metav1.TypeMeta{APIVersion: cappv1alpha1.GroupVersion.String(), Kind: "CappList"}, Items: []cappv1alpha1.Capp{}}
						case "/api/v1/namespaces/team-a/configmaps":
							effects.Add(1)
							body = corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMapList"}, Items: []corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "item", Namespace: "team-a", Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}}}}}
						case "/api/v1/namespaces/team-a/configmaps/item":
							effects.Add(1)
							body = corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "item", Namespace: "team-a", Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}}}
						case "/api/v1/namespaces/new", "/api/v1/namespaces/capp-system/configmaps/capp-assignment-new":
							effects.Add(1)
							w.WriteHeader(http.StatusNotFound)
							body = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: metav1.Unversioned.String(), Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
						case "/api/v1/namespaces", "/api/v1/namespaces/capp-system/configmaps":
							effects.Add(1)
							var object map[string]any
							if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
								t.Error(err)
								return
							}
							body = object
						default:
							t.Errorf("unexpected resource request %s %s", r.Method, r.URL.Path)
							http.NotFound(w, r)
							return
						}
						if err := json.NewEncoder(w).Encode(body); err != nil {
							t.Error(err)
						}
					})
					router := p.router(t, map[string]requestpolicy.Target{"c1": {Store: store, OperatorGroups: []string{"operators"}}})
					group := router.Group("/api/v1/clusters/:cluster")
					configmaps.New().RegisterRoutes(group)
					newCappsHandler(t, false, nil, nil, config.ObservabilityConfig{}).RegisterRoutes(group)
					admin, err := nshandler.NewAdministration(p.clients["c1"].Resources, "capp-system", nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					nshandler.NewWithAdministration(zap.NewNop(), map[string]*nshandler.Administration{"c1": admin}).RegisterRoutes(group)
					w := policyRequest(router, tc.method, "/api/v1/clusters/c1"+tc.path, tc.body)
					if w.Code != tc.status {
						t.Fatalf("route = %d %s, want %d", w.Code, w.Body.String(), tc.status)
					}
					if got := effects.Load(); got != int64(tc.effects) {
						t.Errorf("dependent effects = %d, want %d", got, tc.effects)
					}
					if tc.path == "/configmaps" {
						var response struct {
							Total int `json:"total"`
						}
						if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						if response.Total != tc.effects {
							t.Errorf("aggregate total = %d, want %d", response.Total, tc.effects)
						}
					}
				})
			}
		})
	}
}

func TestBackendPolicyProviderAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, target, header, mode string
		rejected                   bool
		reviewStatus               int64
		status                     int
		code, message              string
		calls                      int64
	}{
		{name: "missing bearer before unknown target", target: "missing", status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: auth.ErrUnauthenticated.Error()},
		{name: "malformed bearer before unknown target", target: "missing", header: "Bearer two tokens", status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: auth.ErrUnauthenticated.Error()},
		{name: "unknown passthrough target", mode: config.AuthModePassthrough, target: "missing", header: "Bearer caller-token", status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: auth.ErrUnauthenticated.Error()},
		{name: "verified identity for unknown target", mode: config.AuthModeOpenShift, target: "missing", header: "Bearer caller-token", status: http.StatusNotFound, code: apierrors.CodeClusterNotFound, calls: 1},
		{name: "rejected token for unknown target", mode: config.AuthModeOpenShift, target: "missing", header: "Bearer caller-token", rejected: true, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: auth.ErrUnauthenticated.Error(), calls: 1},
		{name: "reviewer unavailable for unknown target", mode: config.AuthModeOpenShift, target: "missing", header: "Bearer caller-token", reviewStatus: http.StatusForbidden, status: http.StatusServiceUnavailable, code: apierrors.CodeInternal, message: auth.ErrIdentityUnavailable.Error(), calls: 1},
		{name: "rejected token", target: "c1", header: "Bearer caller-token", rejected: true, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: auth.ErrUnauthenticated.Error(), calls: 1},
		{name: "reviewer permission unavailable", target: "c1", header: "Bearer caller-token", reviewStatus: http.StatusForbidden, status: http.StatusServiceUnavailable, code: apierrors.CodeInternal, message: auth.ErrIdentityUnavailable.Error(), calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modes := []string{config.AuthModePassthrough, config.AuthModeOpenShift}
			if tc.mode != "" {
				modes = []string{tc.mode}
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					p := newPolicyProvider(t, mode, map[string]authv1.UserInfo{"c1": {Username: "alice"}}, nil)
					if tc.rejected {
						p.reviews["c1"].user.Store(nil)
					}
					p.reviews["c1"].status.Store(tc.reviewStatus)
					if tc.header == "" || tc.header == "Bearer two tokens" {
						p.manager.GetFn = func(string) (*cluster.ClusterClient, error) {
							t.Error("malformed credential reached target lookup")
							return nil, cluster.ErrClusterNotFound
						}
					}
					router := p.router(t, map[string]requestpolicy.Target{"c1": {}})
					req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/"+tc.target, nil)
					req.Header.Set("Authorization", tc.header)
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					var response struct {
						Error apierrors.APIError `json:"error"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if w.Code != tc.status || response.Error.Code != tc.code {
						t.Fatalf("response = %d %+v, want %d %s", w.Code, response.Error, tc.status, tc.code)
					}
					if tc.message != "" && response.Error.Message != tc.message {
						t.Errorf("message = %q, want %q", response.Error.Message, tc.message)
					}
					if got := p.reviews["c1"].calls.Load(); got != tc.calls {
						t.Errorf("reviews = %d, want %d", got, tc.calls)
					}
				})
			}
		})
	}
}

func TestBackendPolicyTargetlessDenial(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		rejected           bool
		outageTarget       string
		status             int
	}{
		{name: "unclassified route", method: http.MethodGet, path: "/new-api", status: http.StatusForbidden},
		{name: "wrong public method", method: http.MethodPost, path: "/healthz", status: http.StatusForbidden},
		{name: "rejected identity", method: http.MethodGet, path: "/new-api", rejected: true, status: http.StatusUnauthorized},
		{name: "first reviewer outage", method: http.MethodGet, path: "/new-api", outageTarget: "c1", status: http.StatusServiceUnavailable},
		{name: "later reviewer outage", method: http.MethodGet, path: "/new-api", outageTarget: "c2", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPolicyProvider(t, config.AuthModePassthrough, map[string]authv1.UserInfo{"c1": {Username: "alice"}, "c2": {Username: "alice"}}, nil)
			if tc.rejected {
				p.reviews["c1"].user.Store(nil)
				p.reviews["c2"].user.Store(nil)
			}
			if tc.outageTarget != "" {
				p.reviews[tc.outageTarget].status.Store(http.StatusServiceUnavailable)
			}
			r := p.router(t, map[string]requestpolicy.Target{"c1": {}, "c2": {}})
			r.Handle(tc.method, tc.path, func(*gin.Context) { t.Error("denied route reached handler") })
			w := policyRequest(r, tc.method, tc.path, "")
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusForbidden {
				requirePolicyError(t, w, apierrors.CodeForbidden, apierrors.ReasonPolicyDenied)
			}
		})
	}
}

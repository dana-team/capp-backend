package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/api/capps"
	"github.com/dana-team/capp-backend/pkg/k8s"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	"sigs.k8s.io/yaml"
)

func TestNewBackendPolicy(t *testing.T) {
	for _, provider := range []string{config.AuthModeLDAP, config.AuthModePassthrough} {
		t.Run(provider, func(t *testing.T) { testNewBackendPolicy(t, provider) })
	}
}

func testNewBackendPolicy(t *testing.T, provider string) {
	assignment, err := (authorization.Policy{
		Version: authorization.PolicySchemaVersion, Namespace: "team-a",
		Bindings: []authorization.Binding{{Kind: rbacv1.UserKind, Name: "viewer", Role: authorization.RoleViewer}},
	}).ConfigMap("policies", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assignment.UID = "policy-uid"
	assignment.ResourceVersion = "1"
	assignment.Annotations = map[string]string{authorization.NamespaceUIDAnnotation: "uid-a"}
	var synced, created atomic.Bool
	var resourceCalls atomic.Int64
	caller := "operator"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer system-token" || r.Header.Get(transport.ImpersonateUserHeader) != "" || r.Header.Get(transport.ImpersonateGroupHeader) != "" {
			t.Error("all operations must use the backend credential without impersonation")
		}
		w.Header().Set("Content-Type", "application/json")
		ns := corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: "uid-a", Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}}
		var body any
		switch {
		case r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews":
			body = acceptedReview(t, r, caller)
		case r.URL.Query().Get("watch") == "true":
			// A watch outage after LIST must not revoke initial synchronization.
			w.WriteHeader(http.StatusForbidden)
			body = metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden}
		case r.URL.Path == "/api":
			body = metav1.APIVersions{Versions: []string{"v1"}}
		case r.URL.Path == "/apis":
			body = metav1.APIGroupList{}
		case r.URL.Path == "/api/v1":
			body = metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "namespaces", Kind: "Namespace"}, {Name: "configmaps", Kind: "ConfigMap", Namespaced: true}, {Name: "resourcequotas", Kind: "ResourceQuota", Namespaced: true}}}
		case r.URL.Path == "/api/v1/namespaces/team-a":
			body = ns
		case r.URL.Path == "/api/v1/namespaces" && r.Method == http.MethodPost:
			ns.Name = "team-b"
			ns.UID = "created-uid"
			created.Store(true)
			w.WriteHeader(http.StatusCreated)
			body = ns
		case r.URL.Path == "/api/v1/namespaces":
			// Keep informer LIST unavailable while operator requests remain usable.
			if r.URL.Query().Get("labelSelector") == "" && !synced.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				body = metav1.Status{Status: metav1.StatusFailure, Code: http.StatusServiceUnavailable, Reason: metav1.StatusReasonServiceUnavailable}
			} else {
				body = corev1.NamespaceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.Namespace{ns}}
			}
		case r.URL.Path == "/api/v1/namespaces/policies/configmaps":
			body = corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.ConfigMap{*assignment}}
		case r.URL.Path == "/api/v1/namespaces/team-a/configmaps":
			resourceCalls.Add(1)
			body = corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, Items: []corev1.ConfigMap{}}
		default:
			w.WriteHeader(http.StatusNotFound)
			body = metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	cfg, mgr := authorizationConfig(t, upstream.URL)
	cfg.Auth.Mode = provider
	get := mgr.GetFn
	mgr.GetFn = func(name string) (*cluster.ClusterClient, error) {
		cc, err := get(name)
		if err == nil {
			cc.RestConfig.CAData = pem.EncodeToMemory(&pem.Block{Type: cert.CertificateBlockType, Bytes: upstream.Certificate().Raw})
		}
		return cc, err
	}
	var authMgr auth.AuthManager = &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
		return auth.ClusterCredential{ImpersonateUser: caller}, nil
	}}
	if provider == config.AuthModePassthrough {
		authMgr = nil
	}
	srv, err := New(cfg, "dev", authMgr, mgr, policyRegistry(t, true), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopPolicyServer(t, srv) })
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer caller")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, r)
		require.Equalf(t, want, w.Code, "%s %s: %s", method, path, w.Body.String())
		return w
	}
	request(http.MethodGet, "/readyz", "", http.StatusServiceUnavailable)
	w := request(http.MethodGet, "/api/v1/clusters/one/namespaces/team-a", "", http.StatusOK)
	if !strings.Contains(w.Body.String(), "team-a") {
		t.Fatalf("missing namespace response: %s", w.Body.String())
	}
	request(http.MethodGet, "/api/v1/clusters/one/namespaces", "", http.StatusOK)
	request(http.MethodPost, "/api/v1/clusters/one/namespaces", `{"name":"team-b"}`, http.StatusCreated)
	if !created.Load() {
		t.Fatal("namespace was not persisted")
	}
	created.Store(false)
	request(http.MethodPost, "/api/v1/clusters/one/namespaces", `{"name":"team-b","users":["system:restricted"]}`, http.StatusBadRequest)
	if created.Load() {
		t.Fatal("restricted binding reached persistence")
	}
	caller = "ordinary"
	created.Store(false)
	request(http.MethodPost, "/api/v1/clusters/one/namespaces", `{"name":"team-b"}`, http.StatusForbidden)
	if created.Load() {
		t.Fatal("denied request performed storage mutation")
	}
	caller = "viewer"
	w = request(http.MethodGet, "/api/v1/clusters", "", http.StatusOK)
	requireClusterNames(t, w, []string{})
	caller = "ordinary"
	request(http.MethodGet, "/api/v1/clusters/one/configmaps", "", http.StatusServiceUnavailable)
	synced.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for !srv.authorization.ready(mgr) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	request(http.MethodGet, "/readyz", "", http.StatusOK)
	request(http.MethodGet, "/api/v1/clusters", "", http.StatusOK)
	request(http.MethodGet, "/api/v1/clusters/one/configmaps", "", http.StatusOK)
	request(http.MethodGet, "/api/v1/clusters/one/namespaces/team-a/configmaps", "", http.StatusForbidden)
	if got := resourceCalls.Load(); got != 0 {
		t.Fatalf("denied caller made %d resource calls, want 0", got)
	}
	caller = "viewer"
	w = request(http.MethodGet, "/api/v1/clusters", "", http.StatusOK)
	requireClusterNames(t, w, []string{"one"})
	request(http.MethodGet, "/api/v1/clusters/one/namespaces/team-a/configmaps", "", http.StatusOK)
	request(http.MethodGet, "/api/v1/clusters/one/namespaces/policies/configmaps", "", http.StatusForbidden)
	request(http.MethodDelete, "/api/v1/clusters/one/namespaces/team-a/configmaps/item", "", http.StatusForbidden)
	if got := resourceCalls.Load(); got != 1 {
		t.Fatalf("authorized read and denied operations made %d resource calls, want 1", got)
	}
}

func requireClusterNames(t *testing.T, response *httptest.ResponseRecorder, want []string) {
	t.Helper()
	var result struct {
		Items []cluster.ClusterMeta `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Items == nil {
		t.Fatal("cluster list must contain an items array")
	}
	names := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		names = append(names, item.Name)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("cluster names = %v, want %v", names, want)
	}
}

func TestNewAuthorization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, mgr := authorizationConfig(t, "https://127.0.0.1:1")
		second := cfg.Clusters[0]
		second.Name = "two"
		second.Authorization.PolicyNamespace = ""
		cfg.Clusters = append(cfg.Clusters, second)
		if _, err := newAuthorization(cfg, mgr); err == nil {
			t.Fatal("invalid second-target policy namespace must fail startup")
		}
		cfg.Clusters[1].Authorization.PolicyNamespace = cfg.Clusters[0].Authorization.PolicyNamespace
		runtime, err := newAuthorization(cfg, mgr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, reconciler := range runtime.reconcilers {
				reconciler.Close()
			}
		})
		if len(runtime.targets) != 2 || runtime.targets["one"].Store == runtime.targets["two"].Store || runtime.targets["one"].ExecutionClient == runtime.targets["two"].ExecutionClient {
			t.Fatal("targets must have independent stores and execution clients")
		}
	})
}

func authorizationConfig(t *testing.T, host string) (*config.Config, *testutil.MockClusterManager) {
	t.Helper()
	scheme, err := k8s.BuildScheme()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Authorization: config.AuthorizationConfig{Mode: config.AuthorizationModeBackendPolicy, RestrictedBindingNamePrefixes: []string{"system:"}}, Auth: config.AuthConfig{Mode: config.AuthModeLDAP}, Clusters: []config.ClusterConfig{{Name: "one", Authorization: config.ClusterAuthorizationConfig{PolicyNamespace: "policies", OperatorUsers: []string{"operator"}}}}}
	cfg.Resources.Namespaces.Enabled = true
	mgr := &testutil.MockClusterManager{
		GetFn: func(name string) (*cluster.ClusterClient, error) {
			cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: name, Healthy: true}, RestConfig: &rest.Config{Host: host, BearerToken: "system-token"}, Scheme: scheme}
			cc.RestConfig.Impersonate = rest.ImpersonationConfig{UserName: "configured-user", Groups: []string{"configured-group"}}
			cc.SetHealthy(true)
			return cc, nil
		},
		ListFn: func() []cluster.ClusterMeta { return []cluster.ClusterMeta{{Name: "one", Healthy: true}} },
	}
	return cfg, mgr
}

func TestServerStop(t *testing.T) {
	entered, release, workerStopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := &authorizationRuntime{workers: []func(context.Context) error{func(ctx context.Context) error {
		<-ctx.Done()
		close(workerStopped)
		return nil
	}}}
	a.start(zap.NewNop())
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer httpServer.Close()
	srv := &Server{httpServer: httpServer.Config, logger: zap.NewNop(), authorization: a}
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get(httpServer.URL)
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	<-entered
	draining := make(chan struct{})
	srv.httpServer.RegisterOnShutdown(func() { close(draining) })
	stopped := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { stopped <- srv.Stop(ctx) }()
	// Shutdown closes the listener before waiting for the active request.
	<-draining
	select {
	case <-workerStopped:
		t.Error("worker stopped before HTTP drain")
	default:
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Error(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	select {
	case <-workerStopped:
	default:
		t.Fatal("Stop returned before joining worker")
	}
}

func TestAuthorizationRuntimeReady(t *testing.T) {
	synced, _, _ := policyStore(t, nil)
	unsynced, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "policies"})
	if err != nil {
		t.Fatal(err)
	}
	a := &authorizationRuntime{targets: map[string]policy.Target{"synced": {Store: synced}, "unsynced": {Store: unsynced}}}
	for _, tc := range []struct {
		name     string
		clusters []cluster.ClusterMeta
		want     bool
	}{
		{name: "no healthy targets", clusters: []cluster.ClusterMeta{{Name: "synced"}}},
		{name: "health and synchronization must belong to same target", clusters: []cluster.ClusterMeta{{Name: "synced"}, {Name: "unsynced", Healthy: true}}},
		{name: "one ready target suffices", clusters: []cluster.ClusterMeta{{Name: "synced", Healthy: true}, {Name: "unsynced", Healthy: true}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &testutil.MockClusterManager{ListFn: func() []cluster.ClusterMeta { return tc.clusters }}
			if got := a.ready(mgr); got != tc.want {
				t.Fatalf("ready = %t, want %t", got, tc.want)
			}
		})
	}
}

func acceptedReview(t *testing.T, request *http.Request, caller string) authv1.TokenReview {
	t.Helper()
	var review authv1.TokenReview
	if err := json.NewDecoder(request.Body).Decode(&review); err != nil {
		t.Error(err)
	}
	if review.Spec.Token != "caller" || len(review.Spec.Audiences) != 0 {
		t.Error("incorrect review body")
	}
	return authv1.TokenReview{TypeMeta: metav1.TypeMeta{APIVersion: authv1.SchemeGroupVersion.String(), Kind: "TokenReview"}, Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{Username: caller}}}
}

func stopPolicyServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Error(err)
	}
}

func TestNewAuthenticationFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, mgr := authorizationConfig(t, "https://127.0.0.1:1")
		// Invalid local provider settings fail after observation clients have
		// been constructed; their queues must still be released.
		if _, err := New(cfg, "dev", nil, mgr, policyRegistry(t, false), zap.NewNop()); err == nil {
			t.Fatal("invalid LDAP settings did not fail construction")
		}
	})
}

func TestNewProviderConfigurations(t *testing.T) {
	for _, provider := range []string{config.AuthModeLDAP, config.AuthModeOpenShift, config.AuthModePassthrough} {
		t.Run(provider, func(t *testing.T) {
			var reviews atomic.Int64
			var unavailable atomic.Bool
			unavailable.Store(true)
			assignment, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: "team-a", Bindings: []authorization.Binding{{Kind: rbacv1.UserKind, Name: "editor", Role: authorization.RoleEditor}}}).ConfigMap("policies", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			assignment.UID, assignment.ResourceVersion = "policy-uid", "1"
			assignment.Annotations = map[string]string{authorization.NamespaceUIDAnnotation: "namespace-uid"}
			ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: "namespace-uid", Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}}
			destination := ns.DeepCopy()
			destination.Name = "team-b"
			destinationAssignment, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: "team-b", Bindings: []authorization.Binding{{Kind: rbacv1.UserKind, Name: "editor", Role: authorization.RoleEditor}}}).ConfigMap("policies", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			destinationAssignment.UID, destinationAssignment.ResourceVersion = "destination-policy-uid", "1"
			destinationAssignment.Annotations = map[string]string{authorization.NamespaceUIDAnnotation: "namespace-uid"}
			execution := &migrationExecution{targetCluster: "c1", targetNamespace: "team-b", allowed: true}
			resourceHandler := execution.handler(t)
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/.well-known/oauth-authorization-server" {
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("Authorization") != "Bearer c1-backend-token" {
					t.Errorf("upstream authorization = %q, want backend credential", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", runtime.ContentTypeJSON)
				var body any
				switch {
				case r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews":
					reviews.Add(1)
					if unavailable.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					var review authv1.TokenReview
					if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
						t.Error(err)
						return
					}
					body = authv1.TokenReview{TypeMeta: metav1.TypeMeta{APIVersion: authv1.SchemeGroupVersion.String(), Kind: "TokenReview"}, Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{Username: review.Spec.Token}}}
				case r.URL.Query().Get("watch") == "true":
					w.WriteHeader(http.StatusForbidden)
					return
				case r.URL.Path == "/api/v1/namespaces":
					body = corev1.NamespaceList{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "NamespaceList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.Namespace{ns, *destination}}
				case r.URL.Path == "/api/v1/namespaces/policies/configmaps":
					body = corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMapList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.ConfigMap{*assignment, *destinationAssignment}}
				default:
					resourceHandler(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(body); err != nil {
					t.Error(err)
				}
			}))
			defer upstream.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: cert.CertificateBlockType, Bytes: upstream.Certificate().Raw})
			path, tokens := providerStartupConfig(t, provider, upstream.URL, ca, "")
			servers := make([]*Server, 0, 2)
			for range 2 {
				srv := newProviderServer(t, path, &rest.Config{Host: upstream.URL, BearerToken: "c1-backend-token", TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
				servers = append(servers, srv)
			}
			if reviews.Load() != 0 {
				t.Fatalf("startup made %d TokenReviews, want zero", reviews.Load())
			}
			for _, srv := range servers {
				deadline := time.Now().Add(5 * time.Second)
				for !srv.authorization.targets["c1"].Store.Health().Synced {
					if time.Now().After(deadline) {
						t.Fatal("server observation did not synchronize")
					}
					time.Sleep(time.Millisecond)
				}
				startupRequest(t, srv, http.MethodGet, "/readyz", "", "", http.StatusOK)
				if provider != config.AuthModeLDAP {
					startupRequest(t, srv, http.MethodGet, "/api/v1/clusters", "editor", "", http.StatusServiceUnavailable)
				}
			}
			unavailable.Store(false)
			for _, tc := range []struct {
				name, method, path, user string
				body                     *capps.MigrateRequest
				status                   int
			}{
				{name: "allowed", method: http.MethodGet, path: "/api/v1/clusters/c1", user: "editor", status: http.StatusOK},
				{name: "concealed cluster", method: http.MethodGet, path: "/api/v1/clusters/c1", user: "outsider", status: http.StatusNotFound},
				{name: "collection", method: http.MethodGet, path: "/api/v1/clusters", user: "editor", status: http.StatusOK},
				{name: "invisible collection", method: http.MethodGet, path: "/api/v1/clusters", user: "outsider", status: http.StatusOK},
				{name: "allowed migration", method: http.MethodPost, path: "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", body: &capps.MigrateRequest{TargetCluster: "c1", TargetNamespace: "team-b"}, user: "editor", status: http.StatusOK},
				{name: "denied resource", method: http.MethodGet, path: "/api/v1/clusters/c1/namespaces/team-a/configmaps", user: "outsider", status: http.StatusForbidden},
				{name: "concealed migration", method: http.MethodPost, path: "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", body: &capps.MigrateRequest{TargetCluster: "missing", TargetNamespace: "team-a"}, user: "editor", status: http.StatusForbidden},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var body []byte
					if tc.body != nil {
						var err error
						body, err = json.Marshal(tc.body)
						require.NoError(t, err)
					}
					first := startupRequest(t, servers[0], tc.method, tc.path, tokens[tc.user], string(body), tc.status)
					second := startupRequest(t, servers[1], tc.method, tc.path, tokens[tc.user], string(body), tc.status)
					if first != second {
						t.Errorf("independent instance responses differ: %s / %s", first, second)
					}
				})
			}
			if provider == config.AuthModeLDAP && reviews.Load() != 0 {
				t.Fatalf("LDAP made %d reviews, want zero", reviews.Load())
			}
		})
	}
}

func TestNewProviderChartConfigurations(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart-to-server configuration coverage")
	}
	upstream := httptest.NewTLSServer(http.NotFoundHandler())
	defer upstream.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: cert.CertificateBlockType, Bytes: upstream.Certificate().Raw})
	for _, provider := range []string{config.AuthModeLDAP, config.AuthModeOpenShift, config.AuthModePassthrough} {
		t.Run(provider, func(t *testing.T) {
			path, _ := providerStartupConfig(t, provider, upstream.URL, ca, helm)
			newProviderServer(t, path, &rest.Config{Host: upstream.URL, BearerToken: "c1-backend-token", TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
		})
	}
}

func newProviderServer(t *testing.T, path string, home *rest.Config) *Server {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("Validate(%s) = %v", cfg.Auth.Mode, err)
	}
	cc, err := cluster.BuildClusterClient(cfg.Clusters[0], testutil.TestScheme(t))
	if err != nil {
		t.Fatal(err)
	}
	cc.SetHealthy(true)
	mgr := policyManager(t, cc)
	mgr.ListFn = func() []cluster.ClusterMeta {
		meta := cc.Meta
		meta.Healthy = cc.IsHealthy()
		return []cluster.ClusterMeta{meta}
	}
	var manager auth.AuthManager
	if cfg.Auth.Mode == config.AuthModeOpenShift {
		// Supply the existing home-config dependency instead of relying on a
		// service-account mount on the machine running this test.
		manager, err = auth.New(cfg, auth.Dependencies{HomeConfig: home})
		if err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(cfg, "dev", manager, mgr, policyRegistry(t, true), zap.NewNop())
	if err != nil {
		t.Fatalf("New(%s) = %v", cfg.Auth.Mode, err)
	}
	t.Cleanup(func() { stopPolicyServer(t, srv) })
	return srv
}

func startupRequest(t *testing.T, srv *Server, method, path, token, body string, status int) string {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", runtime.ContentTypeJSON)
	response := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(response, req)
	if response.Code != status {
		t.Fatalf("%s %s = %d %s, want %d", method, path, response.Code, response.Body.String(), status)
	}
	return response.Body.String()
}

// providerStartupConfig writes configuration directly, or renders it through
// Helm when supplied, using the same environment-injected secrets in both cases.
func providerStartupConfig(t *testing.T, provider, host string, ca []byte, helm string) (string, map[string]string) {
	t.Helper()
	tokens := map[string]string{"editor": "editor", "outsider": "outsider"}
	authValues := map[string]any{"mode": provider, "openshift": map[string]any{"apiServer": host, "caCert": base64.StdEncoding.EncodeToString(ca), "clientId": "backend", "clientSecret": "", "redirectUri": "https://backend/callback"}}
	secrets := map[string]any{"openshiftClientSecret": "oauth-secret"}
	if provider == config.AuthModeLDAP {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		privateKey := string(pem.EncodeToMemory(&pem.Block{Type: keyutil.PrivateKeyBlockType, Bytes: der}))
		secrets["ldapSigningPrivateKey"] = privateKey
		secrets["ldapBindPassword"] = "bind-secret"
		authValues["ldap"] = map[string]any{"url": "ldaps://directory.example", "bindDN": "cn=backend", "userBaseDN": "ou=users", "userFilter": "(uid={username})", "groupBaseDN": "ou=groups", "issuer": "backend", "audience": "backend", "signingKeyID": "active"}
		for user := range tokens {
			token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"iss": "backend", "aud": "backend", "sub": user, "groups": []string{}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "token_use": "access"})
			token.Header["kid"] = "active"
			tokens[user], err = token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY", privateKey)
		t.Setenv("CAPP_AUTH_LDAP_BINDPASSWORD", "bind-secret")
	}
	t.Setenv("CAPP_AUTH_OPENSHIFT_CLIENTSECRET", "oauth-secret")
	t.Setenv("CAPP_CLUSTERS_0_CREDENTIAL_INLINE_TOKEN", "c1-backend-token")
	configuration := map[string]any{
		"auth":          authValues,
		"authorization": map[string]any{"mode": config.AuthorizationModeBackendPolicy, "restrictedBindingNamePrefixes": []string{}},
		"clusters":      []any{map[string]any{"name": "c1", "credential": map[string]any{"inline": map[string]any{"apiServer": host, "caCert": base64.StdEncoding.EncodeToString(ca)}}, "authorization": map[string]any{"policyNamespace": "policies"}}},
	}
	data, err := yaml.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if helm != "" {
		data = renderProviderConfig(t, helm, configuration, secrets)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path, tokens
}

func renderProviderConfig(t *testing.T, helm string, configuration, secrets map[string]any) []byte {
	t.Helper()
	values, err := json.Marshal(map[string]any{"secret": secrets, "config": configuration})
	if err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(valuesPath, values, 0600); err != nil {
		t.Fatal(err)
	}
	rendered, err := exec.CommandContext(t.Context(), helm, "template", "startup", "../../helm/capp-backend", "-f", valuesPath, "--show-only", "templates/configmap.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, rendered)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(rendered, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["config.yaml"] == "" {
		t.Fatal("rendered ConfigMap has no config.yaml")
	}
	return []byte(cm.Data["config.yaml"])
}

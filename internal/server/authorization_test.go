package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/dana-team/capp-backend/pkg/k8s"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNewBackendPolicy(t *testing.T) {
	assignment, err := (authorization.Policy{
		Version: authorization.PolicySchemaVersion, Namespace: "team-a",
		Bindings: []authorization.Binding{{Kind: rbacv1.UserKind, Name: "viewer", Role: authorization.RoleViewer}},
	}).ConfigMap("one", "policies", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assignment.UID = "policy-uid"
	assignment.ResourceVersion = "1"
	assignment.Annotations = map[string]string{authorization.NamespaceUIDAnnotation: "uid-a"}
	var synced, created atomic.Bool
	var resourceCalls atomic.Int64
	handler := policyAPIHandler(t, assignment, &synced, &created, &resourceCalls)
	apiServer := func(token string, policyStorage bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get(transport.ImpersonateUserHeader) != "" || r.Header.Get(transport.ImpersonateGroupHeader) != "" {
				t.Error("operation used the wrong cluster credential or impersonation")
			}
			if strings.HasPrefix(r.URL.Path, "/api/v1/namespaces") {
				isPolicy := r.URL.Path == "/api/v1/namespaces/policies" || strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/policies/")
				if isPolicy != policyStorage {
					t.Errorf("request routed to wrong cluster: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			handler.ServeHTTP(w, r)
		}))
	}
	upstream := apiServer("system-token", false)
	t.Cleanup(upstream.Close)
	origin := apiServer("origin-token", true)
	t.Cleanup(origin.Close)
	cfg, mgr := authorizationConfig(t, upstream.URL)
	caller := "operator"
	authMgr := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
		return auth.ClusterCredential{ImpersonateUser: caller}, nil
	}}
	srv, err := newServer(cfg, "dev", authMgr, mgr, policyRegistry(t, true), zap.NewNop(), func() (*rest.Config, error) {
		return &rest.Config{Host: origin.URL, BearerToken: "origin-token"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer caller")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
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

func policyAPIHandler(t *testing.T, assignment *corev1.ConfigMap, synced, created *atomic.Bool, resourceCalls *atomic.Int64) http.HandlerFunc {
	t.Helper()
	decoder := serializer.NewCodecFactory(testutil.TestScheme(t)).UniversalDeserializer()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", runtime.ContentTypeJSON)
		ns := corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: "uid-a", Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}}
		var body any
		switch {
		case r.URL.Query().Get("watch") == "true":
			// A watch outage after LIST must not revoke initial synchronization.
			w.WriteHeader(http.StatusForbidden)
			body = metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden}
		case r.URL.Path == "/api":
			body = metav1.APIVersions{Versions: []string{corev1.SchemeGroupVersion.Version}}
		case r.URL.Path == "/apis":
			body = metav1.APIGroupList{}
		case r.URL.Path == "/api/v1":
			body = metav1.APIResourceList{GroupVersion: corev1.SchemeGroupVersion.String(), APIResources: []metav1.APIResource{{Name: "namespaces", Kind: "Namespace"}, {Name: "configmaps", Kind: "ConfigMap", Namespaced: true}, {Name: "resourcequotas", Kind: "ResourceQuota", Namespaced: true}}}
		case r.URL.Path == "/api/v1/namespaces/policies":
			ns.Name = "policies"
			body = ns
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
				body = corev1.NamespaceList{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "NamespaceList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.Namespace{ns}}
			}
		case r.URL.Path == "/api/v1/namespaces/policies/configmaps" && r.Method == http.MethodPost:
			var createdPolicy corev1.ConfigMap
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if _, _, err := decoder.Decode(data, nil, &createdPolicy); err != nil {
				t.Error(err)
				return
			}
			if createdPolicy.Name != authorization.PolicyName("one", "team-b") {
				t.Errorf("unexpected policy name: %q", createdPolicy.Name)
			}
			w.WriteHeader(http.StatusCreated)
			body = createdPolicy
		case r.URL.Path == "/api/v1/namespaces/policies/configmaps":
			body = corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMapList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.ConfigMap{*assignment}}
		case r.URL.Path == "/api/v1/namespaces/team-a/configmaps":
			resourceCalls.Add(1)
			body = corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMapList"}, Items: []corev1.ConfigMap{}}
		default:
			w.WriteHeader(http.StatusNotFound)
			body = metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	})
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
		if _, err := newAuthorization(cfg, mgr, &rest.Config{Host: "https://127.0.0.1:1"}); err == nil {
			t.Fatal("invalid second-target policy namespace must fail startup")
		}
		cfg.Clusters[1].Authorization.PolicyNamespace = cfg.Clusters[0].Authorization.PolicyNamespace
		runtime, err := newAuthorization(cfg, mgr, &rest.Config{Host: "https://127.0.0.1:1"})
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
		ClientForFn: func(*cluster.ClusterClient, auth.ClusterCredential) (client.Client, error) {
			t.Fatal("Kubernetes-mode client factory invoked")
			return nil, nil
		},
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
	unsynced, err := authorization.NewStore(fake.NewClientset(), fake.NewClientset(), authorization.StoreOptions{ClusterName: "c1", PolicyNamespace: "policies"})
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

func TestNewAuthorizationProtectsPolicyNamespaces(t *testing.T) {
	cfg, mgr := authorizationConfig(t, "https://127.0.0.1:1")
	second := cfg.Clusters[0]
	second.Name = "two"
	second.Authorization.PolicyNamespace = "other-policies"
	cfg.Clusters = append(cfg.Clusters, second)
	runtime, err := newAuthorization(cfg, mgr, &rest.Config{Host: "https://127.0.0.1:2"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, reconciler := range runtime.reconcilers {
			reconciler.Close()
		}
	})
	for clusterName, target := range runtime.targets {
		for _, namespace := range []string{"policies", "other-policies"} {
			if err := target.Store.CheckTarget(namespace); !errors.Is(err, authorization.ErrProtectedNamespace) {
				t.Errorf("CheckTarget(%q) on %q = %v, want protected namespace", namespace, clusterName, err)
			}
		}
	}
}

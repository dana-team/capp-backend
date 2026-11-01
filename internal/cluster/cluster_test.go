package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// alwaysOKServer returns a test server that responds 200 to all requests.
func alwaysOKServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	return s
}

func testLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

func inlineClusterConfig(name, apiServer string) config.ClusterConfig {
	return config.ClusterConfig{
		Name: name,
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{APIServer: apiServer, Insecure: true},
		},
	}
}

func inlineClusterConfigWithToken(name, apiServer, token string) config.ClusterConfig {
	return config.ClusterConfig{
		Name: name,
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{APIServer: apiServer, Token: token, Insecure: true},
		},
	}
}

// testClusterClient creates a ClusterClient backed by the given test server.
func testClusterClient(t *testing.T, srv *httptest.Server, token string) *ClusterClient {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return &ClusterClient{
		Meta:       ClusterMeta{Name: "test"},
		RestConfig: mustBuildRestConfig(t, inlineClusterConfigWithToken("test", srv.URL, token)),
		Scheme:     scheme,
	}
}

func testManager(clients map[string]*ClusterClient) *defaultClusterManager {
	return &defaultClusterManager{clusters: clients, logger: testLogger()}
}

// ── BuildRestConfig tests ─────────────────────────────────────────────────────

func TestBuildRestConfig_Inline_NoCA(t *testing.T) {
	restCfg, err := BuildRestConfig(inlineClusterConfigWithToken("dev", "https://localhost:6443", "tok"))
	require.NoError(t, err)
	assert.Equal(t, "https://localhost:6443", restCfg.Host)
	assert.Equal(t, "tok", restCfg.BearerToken)
	assert.True(t, restCfg.Insecure)
	assert.Equal(t, userAgent, restCfg.UserAgent)
}

func TestBuildRestConfig_Inline_WithCA(t *testing.T) {
	encoded := "dGVzdC1jYS1kYXRh" // base64("test-ca-data")
	cfg := config.ClusterConfig{
		Name: "prod",
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{
				APIServer: "https://prod.example.com:6443",
				CACert:    encoded,
				Token:     "prod-token",
			},
		},
	}
	restCfg, err := BuildRestConfig(cfg)
	require.NoError(t, err)
	assert.False(t, restCfg.Insecure)
	assert.Equal(t, []byte("test-ca-data"), restCfg.CAData)
}

func TestBuildRestConfig_NoCredential(t *testing.T) {
	_, err := BuildRestConfig(config.ClusterConfig{Name: "empty", Credential: config.CredentialConfig{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no credential configured")
}

func TestBuildRestConfig_Inline_NoCA_NoInsecure(t *testing.T) {
	cfg := config.ClusterConfig{
		Name: "strict",
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{APIServer: "https://localhost:6443"},
		},
	}
	_, err := BuildRestConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "caCert or explicit insecure")
}

func TestBuildRestConfig_InvalidCACert(t *testing.T) {
	cfg := config.ClusterConfig{
		Name: "bad",
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{
				APIServer: "https://x",
				CACert:    "!!!not-base64!!!",
			},
		},
	}
	_, err := BuildRestConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding base64")
}

// ── IsNamespaceAllowed tests ──────────────────────────────────────────────────

func TestIsNamespaceAllowed_EmptyList(t *testing.T) {
	mgr := &defaultClusterManager{}
	cc := &ClusterClient{Meta: ClusterMeta{AllowedNamespaces: []string{}}}
	assert.True(t, mgr.IsNamespaceAllowed(cc, "anything"))
}

func TestIsNamespaceAllowed_Listed(t *testing.T) {
	mgr := &defaultClusterManager{}
	cc := &ClusterClient{Meta: ClusterMeta{AllowedNamespaces: []string{"ns-a", "ns-b"}}}
	assert.True(t, mgr.IsNamespaceAllowed(cc, "ns-a"))
	assert.True(t, mgr.IsNamespaceAllowed(cc, "ns-b"))
	assert.False(t, mgr.IsNamespaceAllowed(cc, "ns-c"))
}

// ── ClusterManager tests ──────────────────────────────────────────────────────

func TestClusterManager_Get_NotFound(t *testing.T) {
	mgr := testManager(map[string]*ClusterClient{})
	_, err := mgr.Get("nonexistent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterNotFound)
}

func TestClusterManager_List(t *testing.T) {
	ccA := &ClusterClient{Meta: ClusterMeta{Name: "a"}}
	ccA.healthy.Store(true)
	ccB := &ClusterClient{Meta: ClusterMeta{Name: "b"}}
	ccB.healthy.Store(false)
	mgr := testManager(map[string]*ClusterClient{"a": ccA, "b": ccB})
	list := mgr.List()
	assert.Len(t, list, 2)
}

func TestClusterManager_IsAnyHealthy(t *testing.T) {
	ccA := &ClusterClient{Meta: ClusterMeta{}}
	ccA.healthy.Store(false)
	ccB := &ClusterClient{Meta: ClusterMeta{}}
	ccB.healthy.Store(true)
	mgr := testManager(map[string]*ClusterClient{"a": ccA, "b": ccB})
	assert.True(t, mgr.IsAnyHealthy())

	mgr.clusters["b"].healthy.Store(false)
	assert.False(t, mgr.IsAnyHealthy())
}

func TestClusterManager_New_UnreachableClusterNotFatal(t *testing.T) {
	cfgs := []config.ClusterConfig{
		inlineClusterConfig("unreachable", "https://192.0.2.1:6443"),
	}
	mgr, err := New(cfgs, testScheme(t), testLogger())
	require.NoError(t, err)
	cc, err := mgr.Get("unreachable")
	require.NoError(t, err)
	assert.False(t, cc.IsHealthy())
}

func TestClientFor_OverridesBearerToken(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	cc := testClusterClient(t, srv, "base-token")
	mgr := testManager(map[string]*ClusterClient{"test": cc})

	k8sClient, err := mgr.ClientFor(cc, auth.ClusterCredential{BearerToken: "request-specific-token"})
	require.NoError(t, err)
	assert.NotNil(t, k8sClient)
	assert.Equal(t, "base-token", cc.RestConfig.BearerToken)
}

func TestClientFor_EmptyCredentialUsesBaseToken(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	cc := testClusterClient(t, srv, "service-account-token")
	mgr := testManager(map[string]*ClusterClient{"test": cc})

	_, err := mgr.ClientFor(cc, auth.ClusterCredential{})
	require.NoError(t, err)
	assert.Equal(t, "service-account-token", cc.RestConfig.BearerToken)
}

func TestClientFor_Impersonation(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	cc := testClusterClient(t, srv, "sa-token")
	mgr := testManager(map[string]*ClusterClient{"test": cc})
	cred := auth.ClusterCredential{
		ImpersonateUser:   "jane",
		ImpersonateGroups: []string{"developers", "system:authenticated"},
	}

	k8sClient, err := mgr.ClientFor(cc, cred)
	require.NoError(t, err)
	assert.NotNil(t, k8sClient)
	assert.Equal(t, "sa-token", cc.RestConfig.BearerToken)
	assert.Empty(t, cc.RestConfig.Impersonate.UserName)
}

func TestClientFor_NoImpersonation_BackwardCompat(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	cc := testClusterClient(t, srv, "sa-token")
	mgr := testManager(map[string]*ClusterClient{"test": cc})

	_, err := mgr.ClientFor(cc, auth.ClusterCredential{BearerToken: "user-token"})
	require.NoError(t, err)
	assert.Empty(t, cc.RestConfig.Impersonate.UserName)
}

func TestServiceClientFor(t *testing.T) {
	t.Run("reuses the client during concurrent acquisition", func(t *testing.T) {
		srv := alwaysOKServer(t)
		t.Cleanup(srv.Close)
		cc := testClusterClient(t, srv, "service-token")
		mgr := testManager(map[string]*ClusterClient{"test": cc})

		clients := make([]client.WithWatch, 20)
		errs := make([]error, len(clients))
		var workers sync.WaitGroup
		for i := range clients {
			workers.Go(func() {
				clients[i], errs[i] = mgr.ServiceClientFor(cc)
			})
		}
		workers.Wait()
		for i := range clients {
			require.NoError(t, errs[i])
			require.NotNil(t, clients[i])
			assert.Same(t, clients[0], clients[i])
		}
		reused, err := mgr.ServiceClientFor(cc)
		require.NoError(t, err)
		assert.Same(t, clients[0], reused)
	})

	t.Run("isolates service and caller credentials across targets and watches", func(t *testing.T) {
		headers := make(chan http.Header, 1)
		srv := serviceAPIServer(t, headers)
		first := testClusterClient(t, srv, "first-service-token")
		first.RestConfig.Impersonate = rest.ImpersonationConfig{UserName: "configured-user", Groups: []string{"configured-group"}}
		second := testClusterClient(t, srv, "second-service-token")
		second.Meta.Name = "second"
		mgr := testManager(map[string]*ClusterClient{"test": first, "second": second})

		firstClient, err := mgr.ServiceClientFor(first)
		require.NoError(t, err)
		secondClient, err := mgr.ServiceClientFor(second)
		require.NoError(t, err)
		assert.NotSame(t, firstClient, secondClient)

		callerClient, err := mgr.ClientFor(first, auth.ClusterCredential{
			BearerToken: "caller-token", ImpersonateUser: "alice", ImpersonateGroups: []string{"developers"},
		})
		require.NoError(t, err)
		for _, tc := range []struct {
			client client.Client
			token  string
			user   string
			groups []string
		}{
			{callerClient, "caller-token", "alice", []string{"developers"}},
			{firstClient, "first-service-token", "", nil},
			{secondClient, "second-service-token", "", nil},
		} {
			require.NoError(t, tc.client.List(t.Context(), &corev1.NamespaceList{}))
			got := <-headers
			assert.Equal(t, "Bearer "+tc.token, got.Get("Authorization"))
			assert.Equal(t, tc.user, got.Get("Impersonate-User"))
			assert.Equal(t, tc.groups, got.Values("Impersonate-Group"))
		}

		watchCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		stream, err := firstClient.Watch(watchCtx, &corev1.NamespaceList{})
		require.NoError(t, err)
		t.Cleanup(stream.Stop)
		got := <-headers
		assert.Equal(t, "Bearer first-service-token", got.Get("Authorization"))
		assert.Empty(t, got.Get("Impersonate-User"))
		assert.Empty(t, got.Values("Impersonate-Group"))
		select {
		case event := <-stream.ResultChan():
			assert.Equal(t, watch.Added, event.Type)
		case <-watchCtx.Done():
			t.Fatalf("service watch did not return the namespace event: %v", watchCtx.Err())
		}
		assert.Equal(t, "first-service-token", first.RestConfig.BearerToken)
		assert.Equal(t, "configured-user", first.RestConfig.Impersonate.UserName)
		assert.Equal(t, []string{"configured-group"}, first.RestConfig.Impersonate.Groups)
	})

	t.Run("retries construction failures and preserves token-file credentials", func(t *testing.T) {
		headers := make(chan http.Header, 1)
		srv := serviceAPIServer(t, headers)
		cc := testClusterClient(t, srv, "")
		tokenPath := filepath.Join(t.TempDir(), "token")
		cc.RestConfig.BearerTokenFile = tokenPath
		mgr := testManager(map[string]*ClusterClient{"test": cc})

		_, err := mgr.ServiceClientFor(cc)
		require.ErrorContains(t, err, `cluster "test": creating service client`)
		require.NoError(t, os.WriteFile(tokenPath, []byte("file-service-token"), 0o600))
		serviceClient, err := mgr.ServiceClientFor(cc)
		require.NoError(t, err)
		callerClient, err := mgr.ClientFor(cc, auth.ClusterCredential{BearerToken: "caller-token"})
		require.NoError(t, err)
		require.NoError(t, callerClient.List(t.Context(), &corev1.NamespaceList{}))
		assert.Equal(t, "Bearer caller-token", (<-headers).Get("Authorization"))
		require.NoError(t, serviceClient.List(t.Context(), &corev1.NamespaceList{}))
		assert.Equal(t, "Bearer file-service-token", (<-headers).Get("Authorization"))
		assert.Equal(t, tokenPath, cc.RestConfig.BearerTokenFile)
		assert.Empty(t, cc.RestConfig.BearerToken)
	})
}

// serviceAPIServer records credentials on resource reads and watches.
func serviceAPIServer(t *testing.T, headers chan<- http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/api":
			body = metav1.APIVersions{Versions: []string{"v1"}}
		case "/apis":
			body = metav1.APIGroupList{}
		case "/api/v1":
			body = metav1.APIResourceList{
				GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "namespaces", Kind: "Namespace"}},
			}
		case "/api/v1/namespaces":
			headers <- r.Header.Clone()
			if r.URL.Query().Get("watch") == "true" {
				body = map[string]any{"type": watch.Added, "object": corev1.Namespace{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "team"},
				}}
			} else {
				body = corev1.NamespaceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"}}
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode API response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthCheck(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	restCfg := mustBuildRestConfig(t, inlineClusterConfig("ok", srv.URL))
	hc, err := rest.HTTPClientFor(restCfg)
	require.NoError(t, err)
	assert.NoError(t, checkHealth(hc, restCfg.Host))
}

func TestHealthCheck_Unhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	restCfg := mustBuildRestConfig(t, inlineClusterConfig("bad", srv.URL))
	hc, err := rest.HTTPClientFor(restCfg)
	require.NoError(t, err)
	assert.Error(t, checkHealth(hc, restCfg.Host))
}

func TestClusterManager_StartHealthChecks(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	cfg := inlineClusterConfig("healthy", srv.URL)
	cfg.DisplayName = "Healthy"

	mgr, err := New([]config.ClusterConfig{cfg}, testScheme(t), testLogger())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr.StartHealthChecks(ctx, 1)

	assert.True(t, mgr.IsAnyHealthy())
}

// ── BuildClusterClient GitOpsPath tests ───────────────────────────────────────

func TestBuildClusterClient_GitOpsPath(t *testing.T) {
	tests := []struct {
		name           string
		clusterName    string
		gitOpsPath     string
		wantGitOpsPath string
	}{
		{
			name:           "explicit GitOpsPath used",
			clusterName:    "default",
			gitOpsPath:     "site1",
			wantGitOpsPath: "site1",
		},
		{
			name:           "fallback to cluster Name",
			clusterName:    "production",
			wantGitOpsPath: "production",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := inlineClusterConfig(tt.clusterName, "https://api.example.com")
			cfg.GitOpsPath = tt.gitOpsPath
			cc, err := BuildClusterClient(cfg, testScheme(t))
			require.NoError(t, err)
			assert.Equal(t, tt.wantGitOpsPath, cc.Meta.GitOpsPath)
		})
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func mustBuildRestConfig(t *testing.T, cfg config.ClusterConfig) *rest.Config {
	t.Helper()
	rc, err := BuildRestConfig(cfg)
	require.NoError(t, err)
	return rc
}

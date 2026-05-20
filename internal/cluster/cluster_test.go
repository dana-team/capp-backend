package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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
			gitOpsPath:     "nova",
			wantGitOpsPath: "nova",
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

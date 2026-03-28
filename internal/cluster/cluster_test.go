package cluster

import (
	"context"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"testing"
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

// ── BuildRestConfig tests ─────────────────────────────────────────────────────

func TestBuildRestConfig_Inline_NoCA(t *testing.T) {
	cfg := config.ClusterConfig{
		Name: "dev",
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{
				APIServer: "https://localhost:6443",
				Token:     "tok",
			},
		},
	}
	restCfg, err := BuildRestConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, "https://localhost:6443", restCfg.Host)
	assert.Equal(t, "tok", restCfg.BearerToken)
	// No CA → insecure mode
	assert.True(t, restCfg.Insecure)
	assert.Equal(t, userAgent, restCfg.UserAgent)
}

func TestBuildRestConfig_Inline_WithCA(t *testing.T) {
	// A minimal valid base64-encoded string (not a real PEM, but enough to test decoding)
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
	cfg := config.ClusterConfig{Name: "empty", Credential: config.CredentialConfig{}}
	_, err := BuildRestConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no credential configured")
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
	mgr := &defaultClusterManager{
		clusters: map[string]*ClusterClient{},
		logger:   testLogger(),
	}
	_, err := mgr.Get("nonexistent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterNotFound)
}

func TestClusterManager_List(t *testing.T) {
	mgr := &defaultClusterManager{
		clusters: map[string]*ClusterClient{
			"a": {Meta: ClusterMeta{Name: "a", Healthy: true}},
			"b": {Meta: ClusterMeta{Name: "b", Healthy: false}},
		},
		logger: testLogger(),
	}
	metas := mgr.List()
	assert.Len(t, metas, 2)
}

func TestClusterManager_IsAnyHealthy(t *testing.T) {
	mgr := &defaultClusterManager{
		clusters: map[string]*ClusterClient{
			"a": {Meta: ClusterMeta{Healthy: false}},
			"b": {Meta: ClusterMeta{Healthy: true}},
		},
	}
	assert.True(t, mgr.IsAnyHealthy())

	mgr.clusters["b"].Meta.Healthy = false
	assert.False(t, mgr.IsAnyHealthy())
}

func TestClusterManager_New_UnreachableClusterNotFatal(t *testing.T) {
	scheme := testScheme(t)
	cfgs := []config.ClusterConfig{
		{
			Name: "unreachable",
			Credential: config.CredentialConfig{
				Inline: &config.InlineCredential{
					APIServer: "https://192.0.2.1:6443", // TEST-NET — unreachable
				},
			},
		},
	}
	// New should succeed even when the cluster is unreachable — the cluster
	// is registered as unhealthy, not fatal.
	mgr, err := New(cfgs, scheme, testLogger())
	require.NoError(t, err)
	cc, err := mgr.Get("unreachable")
	require.NoError(t, err)
	assert.False(t, cc.Meta.Healthy)
}

func TestClientFor_OverridesBearerToken(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	cc := &ClusterClient{
		Meta: ClusterMeta{Name: "test"},
		RestConfig: mustBuildRestConfig(t, config.ClusterConfig{
			Name: "test",
			Credential: config.CredentialConfig{
				Inline: &config.InlineCredential{
					APIServer: srv.URL,
					Token:     "base-token",
				},
			},
		}),
		Scheme: scheme,
	}

	mgr := &defaultClusterManager{clusters: map[string]*ClusterClient{"test": cc}, logger: testLogger()}
	cred := auth.ClusterCredential{BearerToken: "request-specific-token"}

	k8sClient, err := mgr.ClientFor(cc, cred)
	require.NoError(t, err)
	assert.NotNil(t, k8sClient)
	// The base RestConfig must not have been mutated.
	assert.Equal(t, "base-token", cc.RestConfig.BearerToken)
}

func TestClientFor_EmptyCredentialUsesBaseToken(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	cc := &ClusterClient{
		Meta: ClusterMeta{Name: "test"},
		RestConfig: mustBuildRestConfig(t, config.ClusterConfig{
			Name: "test",
			Credential: config.CredentialConfig{
				Inline: &config.InlineCredential{
					APIServer: srv.URL,
					Token:     "service-account-token",
				},
			},
		}),
		Scheme: scheme,
	}

	mgr := &defaultClusterManager{clusters: map[string]*ClusterClient{"test": cc}, logger: testLogger()}
	// Empty credential — base token should be used.
	_, err := mgr.ClientFor(cc, auth.ClusterCredential{})
	require.NoError(t, err)
	assert.Equal(t, "service-account-token", cc.RestConfig.BearerToken)
}

func TestClientFor_Impersonation(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	cc := &ClusterClient{
		Meta: ClusterMeta{Name: "test"},
		RestConfig: mustBuildRestConfig(t, config.ClusterConfig{
			Name: "test",
			Credential: config.CredentialConfig{
				Inline: &config.InlineCredential{
					APIServer: srv.URL,
					Token:     "sa-token",
				},
			},
		}),
		Scheme: scheme,
	}

	mgr := &defaultClusterManager{clusters: map[string]*ClusterClient{"test": cc}, logger: testLogger()}
	cred := auth.ClusterCredential{
		ImpersonateUser:   "jane",
		ImpersonateGroups: []string{"developers", "system:authenticated"},
	}

	k8sClient, err := mgr.ClientFor(cc, cred)
	require.NoError(t, err)
	assert.NotNil(t, k8sClient)
	// Base RestConfig must not be mutated.
	assert.Equal(t, "sa-token", cc.RestConfig.BearerToken)
	assert.Empty(t, cc.RestConfig.Impersonate.UserName)
}

func TestClientFor_NoImpersonation_BackwardCompat(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	cc := &ClusterClient{
		Meta: ClusterMeta{Name: "test"},
		RestConfig: mustBuildRestConfig(t, config.ClusterConfig{
			Name: "test",
			Credential: config.CredentialConfig{
				Inline: &config.InlineCredential{
					APIServer: srv.URL,
					Token:     "sa-token",
				},
			},
		}),
		Scheme: scheme,
	}

	mgr := &defaultClusterManager{clusters: map[string]*ClusterClient{"test": cc}, logger: testLogger()}
	// Empty impersonation fields — should not set Impersonate on rest.Config.
	cred := auth.ClusterCredential{BearerToken: "user-token"}

	_, err := mgr.ClientFor(cc, cred)
	require.NoError(t, err)
	assert.Empty(t, cc.RestConfig.Impersonate.UserName)
}

func TestHealthCheck(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	restCfg := mustBuildRestConfig(t, config.ClusterConfig{
		Name: "ok",
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{APIServer: srv.URL},
		},
	})
	err := checkHealth(restCfg)
	assert.NoError(t, err)
}

func TestHealthCheck_Unhealthy(t *testing.T) {
	// Server returns 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	restCfg := mustBuildRestConfig(t, config.ClusterConfig{
		Name: "bad",
		Credential: config.CredentialConfig{
			Inline: &config.InlineCredential{APIServer: srv.URL},
		},
	})
	err := checkHealth(restCfg)
	assert.Error(t, err)
}

func TestClusterManager_StartHealthChecks(t *testing.T) {
	srv := alwaysOKServer(t)
	defer srv.Close()

	scheme := testScheme(t)
	cfgs := []config.ClusterConfig{
		{
			Name:        "healthy",
			DisplayName: "Healthy",
			Credential: config.CredentialConfig{
				Inline: &config.InlineCredential{APIServer: srv.URL},
			},
		},
	}

	mgr, err := New(cfgs, scheme, testLogger())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so StartHealthChecks exits quickly
	mgr.StartHealthChecks(ctx, 1)

	assert.True(t, mgr.IsAnyHealthy())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func mustBuildRestConfig(t *testing.T, cfg config.ClusterConfig) *rest.Config {
	t.Helper()
	rc, err := BuildRestConfig(cfg)
	require.NoError(t, err)
	return rc
}

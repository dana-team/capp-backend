package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTempConfig writes content to a temp file and returns its path.
// The caller is responsible for os.Remove when done.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "capp-backend-config-*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func TestLoad_Defaults(t *testing.T) {
	// Load with no file — should apply all built-in defaults.
	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, 30, cfg.Server.ReadTimeoutSeconds)
	assert.Equal(t, 30, cfg.Server.WriteTimeoutSeconds)
	assert.Equal(t, 60, cfg.Server.IdleTimeoutSeconds)
	assert.Equal(t, "passthrough", cfg.Auth.Mode)
	assert.Equal(t, 60, cfg.Auth.JWT.TokenTTLMinutes)
	assert.Equal(t, 1440, cfg.Auth.JWT.RefreshTTLMinutes)
	assert.True(t, cfg.Auth.RateLimit.Enabled)
	assert.Equal(t, 20.0, cfg.Auth.RateLimit.RequestsPerSecond)
	assert.Equal(t, 40, cfg.Auth.RateLimit.Burst)
	assert.Equal(t, "info", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
	assert.True(t, cfg.Metrics.Enabled)
	assert.Equal(t, "/metrics", cfg.Metrics.Path)
	assert.Equal(t, "capp-backend", cfg.Tracing.ServiceName)
	assert.Equal(t, 0.1, cfg.Tracing.SampleRate)
	assert.True(t, cfg.Resources.Namespaces.Enabled)
	assert.True(t, cfg.Resources.Capps.Enabled)
}

func TestLoad_FromFile(t *testing.T) {
	yaml := `
server:
  port: 9090
  corsAllowedOrigins:
    - "https://example.com"
auth:
  mode: jwt
  jwt:
    secretKey: "super-secret"
clusters:
  - name: dev
    displayName: "Dev Cluster"
    credential:
      inline:
        apiServer: "https://dev.example.com:6443"
        token: "dev-token"
`
	path := writeTempConfig(t, yaml)
	defer func() { _ = os.Remove(path) }()

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, []string{"https://example.com"}, cfg.Server.CORSAllowedOrigins)
	assert.Equal(t, "jwt", cfg.Auth.Mode)
	assert.Equal(t, "super-secret", cfg.Auth.JWT.SecretKey)
	require.Len(t, cfg.Clusters, 1)
	assert.Equal(t, "dev", cfg.Clusters[0].Name)
	assert.Equal(t, "Dev Cluster", cfg.Clusters[0].DisplayName)
	assert.Equal(t, "https://dev.example.com:6443", cfg.Clusters[0].Credential.Inline.APIServer)
}

func TestLoad_DisplayNameFallback(t *testing.T) {
	yaml := `
clusters:
  - name: production
    credential:
      inline:
        apiServer: "https://prod.example.com:6443"
        token: "tok"
`
	path := writeTempConfig(t, yaml)
	defer func() { _ = os.Remove(path) }()

	cfg, err := Load(path)
	require.NoError(t, err)
	// DisplayName should fall back to Name when not explicitly set.
	assert.Equal(t, "production", cfg.Clusters[0].DisplayName)
}

func TestLoad_InvalidFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	assert.Error(t, err)
}

// ── Validate tests ───────────────────────────────────────────────────────────

func TestValidate_Happy(t *testing.T) {
	cfg := &Config{
		Auth: AuthConfig{Mode: "passthrough"},
		Clusters: []ClusterConfig{
			{
				Name: "prod",
				Credential: CredentialConfig{
					Inline: &InlineCredential{APIServer: "https://api.example.com"},
				},
			},
		},
	}
	assert.NoError(t, Validate(cfg))
}

func TestValidate_NoClusters(t *testing.T) {
	cfg := &Config{Auth: AuthConfig{Mode: "passthrough"}}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one cluster")
}

func TestValidate_EmptyClusterName(t *testing.T) {
	cfg := &Config{
		Auth: AuthConfig{Mode: "passthrough"},
		Clusters: []ClusterConfig{
			{Name: "", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'name' is required")
}

func TestValidate_DuplicateClusterName(t *testing.T) {
	cred := CredentialConfig{Inline: &InlineCredential{APIServer: "https://x"}}
	cfg := &Config{
		Auth: AuthConfig{Mode: "passthrough"},
		Clusters: []ClusterConfig{
			{Name: "dup", Credential: cred},
			{Name: "dup", Credential: cred},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate cluster name")
}

func TestValidate_NoCredential(t *testing.T) {
	cfg := &Config{
		Auth: AuthConfig{Mode: "passthrough"},
		Clusters: []ClusterConfig{
			{Name: "prod", Credential: CredentialConfig{}},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "either 'kubeconfigPath' or 'inline' must be set")
}

func TestValidate_InlineMissingAPIServer(t *testing.T) {
	cfg := &Config{
		Auth: AuthConfig{Mode: "passthrough"},
		Clusters: []ClusterConfig{
			{Name: "prod", Credential: CredentialConfig{Inline: &InlineCredential{APIServer: ""}}},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'apiServer' is required")
}

func TestValidate_UnknownAuthMode(t *testing.T) {
	cfg := &Config{
		Auth: AuthConfig{Mode: "magic"},
		Clusters: []ClusterConfig{
			{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.mode")
}

func TestValidate_JWTMissingSecretKey(t *testing.T) {
	cfg := &Config{
		Auth: AuthConfig{Mode: "jwt"},
		Clusters: []ClusterConfig{
			{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.jwt.secretKey")
}

func TestValidate_StaticMissingKeys(t *testing.T) {
	cfg := &Config{
		Auth: AuthConfig{Mode: "static", Static: StaticConfig{APIKeys: nil}},
		Clusters: []ClusterConfig{
			{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.static.apiKeys")
}

func TestValidate_MultipleErrors(t *testing.T) {
	// Both empty cluster name and bad auth mode — both should appear.
	cfg := &Config{
		Auth: AuthConfig{Mode: "bad"},
		Clusters: []ClusterConfig{
			{Name: "", Credential: CredentialConfig{KubeconfigPath: "/etc/k"}},
		},
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'name' is required")
	assert.Contains(t, err.Error(), "auth.mode")
}

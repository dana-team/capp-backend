package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const authModeJWT = "jwt"

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

func loadTempConfig(t *testing.T, yaml string) *Config {
	t.Helper()
	path := writeTempConfig(t, yaml)
	defer func() { _ = os.Remove(path) }()
	cfg, err := Load(path)
	require.NoError(t, err)
	return cfg
}

func validInlineCredential() CredentialConfig {
	return CredentialConfig{
		Inline: &InlineCredential{APIServer: "https://api.example.com"},
	}
}

func validClusterConfig(name string) ClusterConfig {
	return ClusterConfig{Name: name, Credential: validInlineCredential()}
}

func passthroughConfig(clusters ...ClusterConfig) *Config {
	if len(clusters) == 0 {
		clusters = []ClusterConfig{validClusterConfig("prod")}
	}
	return &Config{
		Auth:     AuthConfig{Mode: "passthrough"},
		Clusters: clusters,
	}
}

func validOpenShiftConfig() OpenShiftConfig {
	return OpenShiftConfig{
		APIServer:    "https://api.ocp.example.com:6443",
		ClientID:     "capp-backend",
		ClientSecret: "secret",
		RedirectURI:  "https://capp.example.com/callback",
	}
}

func validGitOpsConfig() GitOpsConfig {
	return GitOpsConfig{
		Enabled:    true,
		RepoURL:    "https://github.com/org/repo.git",
		AuthMethod: "token",
		Token:      "tok",
	}
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
	cfg := loadTempConfig(t, `
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
`)
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, []string{"https://example.com"}, cfg.Server.CORSAllowedOrigins)
	assert.Equal(t, authModeJWT, cfg.Auth.Mode)
	assert.Equal(t, "super-secret", cfg.Auth.JWT.SecretKey)
	require.Len(t, cfg.Clusters, 1)
	assert.Equal(t, "dev", cfg.Clusters[0].Name)
	assert.Equal(t, "Dev Cluster", cfg.Clusters[0].DisplayName)
	assert.Equal(t, "https://dev.example.com:6443", cfg.Clusters[0].Credential.Inline.APIServer)
}

func TestLoad_DisplayNameFallback(t *testing.T) {
	cfg := loadTempConfig(t, `
clusters:
  - name: production
    credential:
      inline:
        apiServer: "https://prod.example.com:6443"
        token: "tok"
`)
	assert.Equal(t, "production", cfg.Clusters[0].DisplayName)
}

func TestLoad_InvalidFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	assert.Error(t, err)
}

// ── Validate tests ───────────────────────────────────────────────────────────

func TestValidate_Happy(t *testing.T) {
	assert.NoError(t, Validate(passthroughConfig()))
}

func TestValidate_NoClusters(t *testing.T) {
	cfg := &Config{Auth: AuthConfig{Mode: "passthrough"}}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one cluster")
}

func TestValidate_EmptyClusterName(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}})
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'name' is required")
}

func TestValidate_DuplicateClusterName(t *testing.T) {
	c := validClusterConfig("dup")
	cfg := passthroughConfig(c, c)
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate cluster name")
}

func TestValidate_NoCredential(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "prod", Credential: CredentialConfig{}})
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "either 'kubeconfigPath' or 'inline' must be set")
}

func TestValidate_InlineMissingAPIServer(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{
		Name:       "prod",
		Credential: CredentialConfig{Inline: &InlineCredential{APIServer: ""}},
	})
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'apiServer' is required")
}

func TestValidate_UnknownAuthMode(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}})
	cfg.Auth.Mode = "magic"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.mode")
}

func TestValidate_JWTMissingSecretKey(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}})
	cfg.Auth.Mode = authModeJWT
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.jwt.secretKey")
}

func TestValidate_JWTSecretKeyTooShort(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}})
	cfg.Auth.Mode = authModeJWT
	cfg.Auth.JWT.SecretKey = "short"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 32 characters")
}

func TestValidate_JWTSecretKeyValid(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}})
	cfg.Auth.Mode = authModeJWT
	cfg.Auth.JWT.SecretKey = "this-secret-key-is-at-least-32-bytes-long"
	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_StaticMissingKeys(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "prod", Credential: CredentialConfig{KubeconfigPath: "/etc/kubeconfig"}})
	cfg.Auth.Mode = "static"
	cfg.Auth.Static = StaticConfig{APIKeys: nil}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.static.apiKeys")
}

func TestValidate_OpenShift_Valid(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{
		Name: "home",
		Credential: CredentialConfig{
			Inline: &InlineCredential{
				APIServer: "https://kubernetes.default.svc",
				Token:     "sa-token",
			},
		},
	})
	cfg.Auth = AuthConfig{
		Mode:      "openshift",
		OpenShift: validOpenShiftConfig(),
	}
	assert.NoError(t, Validate(cfg))
}

func TestValidate_OpenShift_MissingRequiredFields(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{
		Name: "home",
		Credential: CredentialConfig{
			Inline: &InlineCredential{
				APIServer: "https://kubernetes.default.svc",
				Token:     "sa-token",
			},
		},
	})
	cfg.Auth = AuthConfig{Mode: "openshift", OpenShift: OpenShiftConfig{}}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.openshift.apiServer")
	assert.Contains(t, err.Error(), "auth.openshift.clientId")
	assert.Contains(t, err.Error(), "auth.openshift.clientSecret")
	assert.Contains(t, err.Error(), "auth.openshift.redirectUri")
}

func TestValidate_OpenShift_ClusterMissingToken(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{
		Name: "home",
		Credential: CredentialConfig{
			Inline: &InlineCredential{
				APIServer: "https://kubernetes.default.svc",
			},
		},
	})
	cfg.Auth = AuthConfig{
		Mode:      "openshift",
		OpenShift: validOpenShiftConfig(),
	}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential.inline.token is required")
}

func TestValidate_OpenShift_Defaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, []string{"user:info", "user:check-access"}, cfg.Auth.OpenShift.Scopes)
	assert.Equal(t, 60, cfg.Auth.OpenShift.TokenCacheTTLSeconds)
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := passthroughConfig(ClusterConfig{Name: "", Credential: CredentialConfig{KubeconfigPath: "/etc/k"}})
	cfg.Auth.Mode = "bad"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'name' is required")
	assert.Contains(t, err.Error(), "auth.mode")
}

// ── GitOpsPath / GitOps config tests ─────────────────────────────────────────

func TestLoad_GitOpsPath(t *testing.T) {
	tests := []struct {
		name           string
		yaml           string
		wantGitOpsPath string
	}{
		{
			name: "fallback to cluster name",
			yaml: `
clusters:
  - name: production
    credential:
      inline:
        apiServer: "https://prod.example.com:6443"
        token: "tok"
`,
			wantGitOpsPath: "production",
		},
		{
			name: "explicit gitOpsPath",
			yaml: `
clusters:
  - name: default
    gitOpsPath: nova
    credential:
      inline:
        apiServer: "https://prod.example.com:6443"
        token: "tok"
`,
			wantGitOpsPath: "nova",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadTempConfig(t, tt.yaml)
			assert.Equal(t, tt.wantGitOpsPath, cfg.Clusters[0].GitOpsPath)
		})
	}
}

func TestLoad_PathPrefixDefault(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "sites", cfg.GitOps.PathPrefix)
}

func TestValidate_GitOps(t *testing.T) {
	tests := []struct {
		name       string
		gitops     GitOpsConfig
		wantErr    bool
		errContain string
	}{
		{
			name:   "valid config",
			gitops: validGitOpsConfig(),
		},
		{
			name: "missing repoURL",
			gitops: GitOpsConfig{
				Enabled: true, AuthMethod: "token", Token: "tok",
			},
			wantErr:    true,
			errContain: "gitops.repoURL is required",
		},
		{
			name: "missing token",
			gitops: GitOpsConfig{
				Enabled: true, RepoURL: "https://github.com/org/repo.git",
				AuthMethod: "token",
			},
			wantErr:    true,
			errContain: "gitops.token is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validClusterConfig("prod")
			c.GitOpsPath = "nova"
			cfg := passthroughConfig(c)
			cfg.GitOps = tt.gitops
			err := Validate(cfg)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContain)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidate_GitOpsPath(t *testing.T) {
	tests := []struct {
		name       string
		gitops     GitOpsConfig
		gitOpsPath string
		wantErr    bool
		errContain string
	}{
		{
			name:       "slash rejected",
			gitops:     validGitOpsConfig(),
			gitOpsPath: "bad/name",
			wantErr:    true,
			errContain: "must not contain slashes or spaces",
		},
		{
			name:       "space rejected",
			gitops:     validGitOpsConfig(),
			gitOpsPath: "bad name",
			wantErr:    true,
			errContain: "must not contain slashes or spaces",
		},
		{
			name:       "skipped when gitops disabled",
			gitops:     GitOpsConfig{Enabled: false},
			gitOpsPath: "bad/name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validClusterConfig("prod")
			c.GitOpsPath = tt.gitOpsPath
			cfg := passthroughConfig(c)
			cfg.GitOps = tt.gitops
			err := Validate(cfg)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContain)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

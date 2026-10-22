package config

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTempConfig writes content to a temp file and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "capp-backend-config-*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func loadTempConfig(t *testing.T, yaml string) *Config {
	t.Helper()
	path := writeTempConfig(t, yaml)
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
		Auth:          AuthConfig{Mode: "passthrough"},
		Authorization: AuthorizationConfig{Mode: AuthorizationModeKubernetes},
		Clusters:      clusters,
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
		Enabled:        true,
		RepoURL:        "https://github.com/org/repo.git",
		AuthMethod:     "token",
		Token:          "tok",
		TimeoutSeconds: 20,
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
	assert.Equal(t, AuthorizationModeKubernetes, cfg.Authorization.Mode)
	assert.True(t, cfg.Auth.RateLimit.Enabled)
	assert.InDelta(t, 20.0, cfg.Auth.RateLimit.RequestsPerSecond, 0.0001)
	assert.Equal(t, 40, cfg.Auth.RateLimit.Burst)
	assert.Equal(t, 10_000, cfg.Auth.RateLimit.MaxIPTracked)
	assert.Equal(t, "info", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
	assert.True(t, cfg.Metrics.Enabled)
	assert.Equal(t, "/metrics", cfg.Metrics.Path)
	assert.Equal(t, "capp-backend", cfg.Tracing.ServiceName)
	assert.InDelta(t, 0.1, cfg.Tracing.SampleRate, 0.0001)
	assert.True(t, cfg.Resources.Namespaces.Enabled)
	assert.True(t, cfg.Resources.Capps.Enabled)
	assert.True(t, cfg.Resources.Capprevisions.Enabled)

	// Benchmark defaults
	assert.False(t, cfg.Resources.Benchmarks.Enabled)
	assert.Empty(t, cfg.Resources.Benchmarks.Image)
	assert.Empty(t, cfg.Resources.Benchmarks.VMURLs)
}

func TestLoad_FromFile(t *testing.T) {
	cfg := loadTempConfig(t, `
server:
  port: 9090
  corsAllowedOrigins:
    - "https://example.com"
auth:
  mode: passthrough
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
	assert.Equal(t, "passthrough", cfg.Auth.Mode)
	require.Len(t, cfg.Clusters, 1)
	assert.Equal(t, "dev", cfg.Clusters[0].Name)
	assert.Equal(t, "Dev Cluster", cfg.Clusters[0].DisplayName)
	assert.Equal(t, "https://dev.example.com:6443", cfg.Clusters[0].Credential.Inline.APIServer)
}

func TestLoad_ClusterTokenEnvOverride(t *testing.T) {
	t.Setenv("CAPP_CLUSTERS_1_CREDENTIAL_INLINE_TOKEN", "injected-token")

	cfg := loadTempConfig(t, `
clusters:
  - name: local
    credential:
      inline:
        apiServer: "https://kubernetes.default.svc"
        token: "local-token"
  - name: remote
    credential:
      inline:
        apiServer: "https://remote.example.com:6443"
        token: ""
`)
	require.Len(t, cfg.Clusters, 2)
	assert.Equal(t, "local-token", cfg.Clusters[0].Credential.Inline.Token)
	assert.Equal(t, "injected-token", cfg.Clusters[1].Credential.Inline.Token)
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

func TestValidate_RateLimitMaxIPTrackedZero(t *testing.T) {
	cfg := passthroughConfig()
	cfg.Auth.RateLimit = RateLimitConfig{Enabled: true, RequestsPerSecond: 20, Burst: 40, MaxIPTracked: 0}
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxIPTracked must be greater than 0")
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
    gitOpsPath: default
    credential:
      inline:
        apiServer: "https://prod.example.com:6443"
        token: "tok"
`,
			wantGitOpsPath: "default",
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

func TestLoad_GitOpsTimeoutDefault(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 20, cfg.GitOps.TimeoutSeconds)
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
				Enabled: true, AuthMethod: "token", Token: "tok", TimeoutSeconds: 20,
			},
			wantErr:    true,
			errContain: "gitops.repoURL is required",
		},
		{
			name: "missing token",
			gitops: GitOpsConfig{
				Enabled: true, RepoURL: "https://github.com/org/repo.git",
				AuthMethod: "token", TimeoutSeconds: 20,
			},
			wantErr:    true,
			errContain: "gitops.token is required",
		},
		{
			name: "non-positive timeout",
			gitops: GitOpsConfig{
				Enabled: true, RepoURL: "https://github.com/org/repo.git",
				AuthMethod: "token", Token: "tok",
			},
			wantErr:    true,
			errContain: "gitops.timeoutSeconds must be greater than 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validClusterConfig("prod")
			c.GitOpsPath = "default"
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

// ── Benchmark config tests ───────────────────────────────────────────────────

func TestValidate_Benchmarks(t *testing.T) {
	tests := []struct {
		name       string
		benchmarks BenchmarkConfig
		wantErr    bool
		errContain string
	}{
		{
			name:       "disabled — no validation",
			benchmarks: BenchmarkConfig{Enabled: false},
		},
		{
			name: "enabled with image and vmURLs — valid",
			benchmarks: BenchmarkConfig{
				Enabled: true,
				Image:   "ghcr.io/dana-team/capp-benchmark-runner:0.1.0",
				VMURLs: []string{
					"http://vm-a.monitoring.svc:8428",
					"http://vm-b.monitoring.svc:8428",
				},
			},
		},
		{
			name: "enabled without image — error",
			benchmarks: BenchmarkConfig{
				Enabled: true,
				VMURLs:  []string{"http://vm-a.monitoring.svc:8428"},
			},
			wantErr:    true,
			errContain: "resources.benchmarks.image is required",
		},
		{
			name: "enabled without vmURLs — error",
			benchmarks: BenchmarkConfig{
				Enabled: true,
				Image:   "ghcr.io/dana-team/capp-benchmark-runner:0.1.0",
			},
			wantErr:    true,
			errContain: "resources.benchmarks.vmURLs is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := passthroughConfig()
			cfg.Resources.Benchmarks = tt.benchmarks
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

func TestLoad_BenchmarkFromFile(t *testing.T) {
	cfg := loadTempConfig(t, `
clusters:
  - name: dev
    credential:
      inline:
        apiServer: "https://dev.example.com:6443"
resources:
  benchmarks:
    enabled: true
    image: "ghcr.io/dana-team/capp-benchmark-runner:0.1.0"
    vmURLs:
      - "http://vm-a.monitoring.svc:8428"
      - "http://vm-b.monitoring.svc:8428"
`)
	assert.True(t, cfg.Resources.Benchmarks.Enabled)
	assert.Equal(t, "ghcr.io/dana-team/capp-benchmark-runner:0.1.0", cfg.Resources.Benchmarks.Image)
	assert.Equal(t, []string{
		"http://vm-a.monitoring.svc:8428",
		"http://vm-b.monitoring.svc:8428",
	}, cfg.Resources.Benchmarks.VMURLs)
}

func TestLoad_BenchmarkVMURLsFromEnv(t *testing.T) {
	t.Setenv("CAPP_RESOURCES_BENCHMARKS_VMURLS", "http://vm-a.monitoring.svc:8428, http://vm-b.monitoring.svc:8428")
	cfg := loadTempConfig(t, `
clusters:
  - name: dev
    credential:
      inline:
        apiServer: "https://dev.example.com:6443"
resources:
  benchmarks:
    enabled: true
    image: "ghcr.io/dana-team/capp-benchmark-runner:0.1.0"
`)
	assert.Equal(t, []string{
		"http://vm-a.monitoring.svc:8428",
		"http://vm-b.monitoring.svc:8428",
	}, cfg.Resources.Benchmarks.VMURLs)
}

func TestLoad(t *testing.T) {
	t.Run("authorization mode configuration", func(t *testing.T) {
		for _, tc := range []struct {
			name, yaml, env, want string
		}{
			{name: "file", yaml: "authorization:\n  mode: application\n", want: AuthorizationModeApplication},
			{name: "environment only", env: AuthorizationModeApplication, want: AuthorizationModeApplication},
			{name: "environment overrides file", yaml: "authorization:\n  mode: application\n", env: AuthorizationModeKubernetes, want: AuthorizationModeKubernetes},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("CAPP_AUTHORIZATION_MODE", tc.env)
				path := ""
				if tc.yaml != "" {
					path = writeTempConfig(t, tc.yaml)
				}
				cfg, err := Load(path)
				if err != nil {
					t.Fatalf("Load(%q): %v", path, err)
				}
				if cfg.Authorization.Mode != tc.want {
					t.Errorf("Load(%q).Authorization.Mode = %q, want %q", path, cfg.Authorization.Mode, tc.want)
				}
			})
		}
	})

	t.Run("per-cluster policy protection", func(t *testing.T) {
		for _, mode := range []string{AuthorizationModeApplication, AuthorizationModeKubernetes} {
			t.Run(mode, func(t *testing.T) {
				t.Setenv("CAPP_AUTHORIZATION_MODE", mode)
				cfg := loadTempConfig(t, `
clusters:
  - name: first
    authorization:
      policyNamespace: policy-one
  - name: second
    allowedNamespaces: [credentials, policy-two]
    authorization:
      policyNamespace: policy-two
      protectedNamespaces: [credentials]
  - name: third
    authorization:
      policyNamespace: policy-three
      protectedNamespaces: [policy-three, backend]
`)
				want := [][]string{nil, {"credentials"}, {"policy-three", "backend"}}
				if mode == AuthorizationModeApplication {
					want[0] = []string{"policy-one"}
					want[1] = []string{"credentials", "policy-two"}
				}
				if len(cfg.Clusters) != len(want) {
					t.Fatalf("Load clusters = %d, want %d", len(cfg.Clusters), len(want))
				}
				for i, cluster := range cfg.Clusters {
					if !slices.Equal(cluster.Authorization.ProtectedNamespaces, want[i]) {
						t.Errorf("Load(%s).Clusters[%d].Authorization.ProtectedNamespaces = %v, want %v", mode, i, cluster.Authorization.ProtectedNamespaces, want[i])
					}
				}
			})
		}
	})

	t.Run("LDAP defaults", func(t *testing.T) {
		cfg, err := Load("")
		require.NoError(t, err)
		assert.Equal(t, LDAPConfig{
			UsernameAttribute:  "sAMAccountName",
			GroupFilter:        "(&(objectClass=group)(member={userDN}))",
			GroupNameAttribute: "sAMAccountName",
			TimeoutSeconds:     10,
			TokenTTLSeconds:    900,
			VerificationKeys:   map[string]string{},
		}, cfg.Auth.LDAP)
	})

	t.Run("LDAP environment configuration", func(t *testing.T) {
		want := validLDAPConfig(t)
		want.CACert = "ca-from-environment"
		want.UsernameAttribute = "uid"
		want.GroupNameAttribute = "cn"
		want.TimeoutSeconds = 20
		want.TokenTTLSeconds = 600
		want.VerificationKeys = map[string]string{}
		t.Setenv("CAPP_AUTH_MODE", AuthModeLDAP)
		for key, value := range map[string]string{
			"URL": want.URL, "CACERT": want.CACert,
			"BINDDN": want.BindDN, "BINDPASSWORD": want.BindPassword,
			"USERBASEDN": want.UserBaseDN, "USERFILTER": want.UserFilter,
			"USERNAMEATTRIBUTE": want.UsernameAttribute,
			"GROUPBASEDN":       want.GroupBaseDN, "GROUPFILTER": want.GroupFilter,
			"GROUPNAMEATTRIBUTE": want.GroupNameAttribute,
			"TIMEOUTSECONDS":     "20", "TOKENTTLSECONDS": "600",
			"ISSUER": want.Issuer, "AUDIENCE": want.Audience,
			"SIGNINGKEYID": want.SigningKeyID, "SIGNINGPRIVATEKEY": want.SigningPrivateKey,
		} {
			t.Setenv("CAPP_AUTH_LDAP_"+key, value)
		}
		cfg, err := Load("")
		require.NoError(t, err)
		assert.Equal(t, AuthModeLDAP, cfg.Auth.Mode)
		assert.Equal(t, want, cfg.Auth.LDAP)
	})

	t.Run("LDAP environment overrides file configuration", func(t *testing.T) {
		want := validLDAPConfig(t)
		want.CACert = "ca-from-file"
		want.UsernameAttribute = "uid"
		want.GroupNameAttribute = "cn"
		want.TimeoutSeconds = 25
		want.TokenTTLSeconds = 1200
		// Surrounding password whitespace is significant, including via environment overrides.
		want.BindPassword = " injected password "
		t.Setenv("CAPP_AUTH_LDAP_BINDPASSWORD", want.BindPassword)
		t.Setenv("CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY", want.SigningPrivateKey)
		cfg := loadTempConfig(t, fmt.Sprintf(`
auth:
  mode: ldap
  ldap:
    url: %q
    caCert: %q
    bindDN: %q
    bindPassword: ""
    userBaseDN: %q
    userFilter: %q
    usernameAttribute: %q
    groupBaseDN: %q
    groupFilter: %q
    groupNameAttribute: %q
    timeoutSeconds: 25
    tokenTTLSeconds: 1200
    issuer: %q
    audience: %q
    signingKeyID: %q
    signingPrivateKey: ""
    verificationKeys:
      key-1: %q
`, want.URL, want.CACert, want.BindDN, want.UserBaseDN, want.UserFilter, want.UsernameAttribute,
			want.GroupBaseDN, want.GroupFilter, want.GroupNameAttribute, want.Issuer, want.Audience,
			want.SigningKeyID, want.VerificationKeys[want.SigningKeyID]))
		assert.Equal(t, want, cfg.Auth.LDAP)
	})

	t.Run("LDAP verification keys", func(t *testing.T) {
		_, publicPEM := ldapSigningKeys(t)
		_, otherPublicPEM := ldapSigningKeys(t)
		cases := []struct {
			name      string
			mode      string
			envMode   string
			settings  string
			wantError bool
		}{
			{name: "accepts lowercase IDs", mode: AuthModeLDAP, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  key-2: %q\n", publicPEM)},
			{name: "rejects uppercase ID", mode: AuthModeLDAP, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  KEY-2: %q\n", publicPEM), wantError: true},
			{name: "rejects case-colliding IDs", mode: AuthModeLDAP, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  KEY-2: %q\n  key-2: %q\n", publicPEM, otherPublicPEM), wantError: true},
			{name: "rejects uppercase ID alongside numeric YAML ID", mode: AuthModeLDAP, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  123: %q\n  KEY-2: %q\n", publicPEM, otherPublicPEM), wantError: true},
			{name: "rejects numeric IDs that normalize identically", mode: AuthModeLDAP, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  123: %q\n  123.0: %q\n", publicPEM, otherPublicPEM), wantError: true},
			{name: "rejects uppercase ID with mixed-case parents", mode: AuthModeLDAP, settings: fmt.Sprintf("AUTH:\n  LDAP:\n    VerificationKeys:\n      KEY-2: %q\n", publicPEM), wantError: true},
			{name: "rejects uppercase dotted ID", mode: AuthModeLDAP, settings: fmt.Sprintf("auth.ldap.verificationKeys.KEY-2: %q\n", publicPEM), wantError: true},
			{name: "environment selects LDAP", mode: AuthModePassthrough, envMode: AuthModeLDAP, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  KEY-2: %q\n", publicPEM), wantError: true},
			{name: "environment selects passthrough", mode: AuthModeLDAP, envMode: AuthModePassthrough, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  KEY-2: %q\n", publicPEM)},
			{name: "OpenShift ignores inactive LDAP keys", mode: AuthModeOpenShift, settings: fmt.Sprintf("auth.ldap.verificationKeys:\n  KEY-2: %q\n", publicPEM)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if tc.envMode != "" {
					t.Setenv("CAPP_AUTH_MODE", tc.envMode)
				}
				path := writeTempConfig(t, fmt.Sprintf("auth.mode: %s\n%s", tc.mode, tc.settings))
				cfg, err := Load(path)
				if tc.wantError {
					require.Error(t, err)
					assert.Contains(t, err.Error(), "auth.ldap.verificationKeys")
					assert.NotContains(t, err.Error(), publicPEM)
					return
				}
				require.NoError(t, err)
				wantMode := tc.mode
				if tc.envMode != "" {
					wantMode = tc.envMode
				}
				require.Equal(t, wantMode, cfg.Auth.Mode)
				if cfg.Auth.Mode == AuthModeLDAP {
					assert.Equal(t, map[string]string{"key-2": publicPEM}, cfg.Auth.LDAP.VerificationKeys)
				}
			})
		}
	})
}

func TestValidateAuthorization(t *testing.T) {
	cases := []struct {
		name      string
		change    func(*Config)
		wantError string
	}{
		{name: "LDAP application", change: func(_ *Config) {}},
		{name: "unknown mode", change: func(c *Config) { c.Authorization.Mode = "unknown" }, wantError: "authorization.mode"},
		{name: "empty mode", change: func(c *Config) { c.Authorization.Mode = "" }, wantError: "authorization.mode"},
		{name: "application with passthrough", change: func(c *Config) { c.Auth.Mode = AuthModePassthrough }, wantError: "requires auth.mode 'ldap'"},
		{name: "application with OpenShift", change: func(c *Config) { c.Auth.Mode = AuthModeOpenShift }, wantError: "requires auth.mode 'ldap'"},
		{name: "missing policy namespace", change: func(c *Config) { c.Clusters[0].Authorization.PolicyNamespace = "" }, wantError: "clusters[0].authorization.policyNamespace"},
		{name: "invalid policy namespace", change: func(c *Config) { c.Clusters[0].Authorization.PolicyNamespace = "policy.example" }, wantError: "clusters[0].authorization.policyNamespace"},
		{name: "empty protected namespace", change: func(c *Config) { c.Clusters[0].Authorization.ProtectedNamespaces = []string{""} }, wantError: "clusters[0].authorization.protectedNamespaces[0]"},
		{name: "invalid protected namespace", change: func(c *Config) { c.Clusters[0].Authorization.ProtectedNamespaces = []string{"credentials", "Invalid"} }, wantError: "clusters[0].authorization.protectedNamespaces[1]"},
		{name: "second cluster missing policy namespace", change: func(c *Config) {
			c.Clusters = append(c.Clusters, validClusterConfig("second"))
			c.Clusters[1].Credential.Inline.Token = "sa-token"
		}, wantError: "clusters[1].authorization.policyNamespace"},
		{name: "missing service-account token", change: func(c *Config) { c.Clusters[0].Credential.Inline.Token = "" }, wantError: "credential.inline.token is required"},
		{name: "protected namespace also allowed", change: func(c *Config) { c.Clusters[0].AllowedNamespaces = []string{"policy", "credentials"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := passthroughConfig()
			cfg.Auth.Mode = AuthModeLDAP
			cfg.Auth.LDAP = validLDAPConfig(t)
			cfg.Auth.OpenShift = validOpenShiftConfig()
			cfg.Authorization.Mode = AuthorizationModeApplication
			cfg.Clusters[0].Credential.Inline.Token = "sa-token"
			cfg.Clusters[0].Authorization = ClusterAuthorizationConfig{
				PolicyNamespace: "policy", ProtectedNamespaces: []string{"credentials"},
			}
			tc.change(cfg)
			err := Validate(cfg)
			if tc.wantError == "" {
				if err != nil {
					t.Errorf("Validate(%s) = %v, want nil", tc.name, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("Validate(%s) = %v, want error containing %q", tc.name, err, tc.wantError)
			}
		})
	}

	t.Run("legacy defaults preserve authentication modes", func(t *testing.T) {
		for _, mode := range []string{AuthModePassthrough, AuthModeOpenShift, AuthModeLDAP} {
			t.Run(mode, func(t *testing.T) {
				cfg := loadTempConfig(t, fmt.Sprintf("auth:\n  mode: %s\nclusters:\n  - name: legacy\n    credential:\n      inline:\n        apiServer: https://api.example.com\n        token: sa-token\n", mode))
				cfg.Auth.LDAP = validLDAPConfig(t)
				cfg.Auth.OpenShift = validOpenShiftConfig()
				cfg.Clusters[0].Authorization = ClusterAuthorizationConfig{
					PolicyNamespace: "Invalid", ProtectedNamespaces: []string{""},
				}
				if err := Validate(cfg); err != nil {
					t.Errorf("Validate(%s with legacy defaults) = %v, want nil", mode, err)
				}
			})
		}
	})
}

func TestValidateLDAP(t *testing.T) {
	privatePEM, publicPEM := ldapSigningKeys(t)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true}
	certificateDER, err := x509.CreateCertificate(rand.Reader, certificate, certificate, publicKey, privateKey)
	require.NoError(t, err)
	caCert := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}))
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	otherPrivateDER, err := x509.MarshalPKCS8PrivateKey(otherKey)
	require.NoError(t, err)
	otherPrivatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherPrivateDER}))
	otherPublicDER, err := x509.MarshalPKIXPublicKey(&otherKey.PublicKey)
	require.NoError(t, err)
	otherPublicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: otherPublicDER}))
	cases := []struct {
		name      string
		change    func(*LDAPConfig)
		wantError string
	}{
		{name: "accepts matching active public key", change: func(*LDAPConfig) {}},
		{name: "accepts derived active public key", change: func(c *LDAPConfig) { c.VerificationKeys = nil }},
		{name: "accepts rotation key", change: func(c *LDAPConfig) { c.VerificationKeys["key-2"] = publicPEM }},
		{name: "accepts default port", change: func(c *LDAPConfig) { c.URL = "ldaps://ad.example.com" }},
		{name: "accepts custom port", change: func(c *LDAPConfig) { c.URL = "ldaps://ad.example.com:1636" }},
		{name: "accepts IPv6 endpoint", change: func(c *LDAPConfig) { c.URL = "ldaps://[::1]:636" }},
		{name: "accepts custom CA", change: func(c *LDAPConfig) { c.CACert = caCert }},
		{name: "rejects empty URL", change: func(c *LDAPConfig) { c.URL = "" }, wantError: "auth.ldap.url"},
		{name: "rejects StartTLS URL", change: func(c *LDAPConfig) { c.URL = "ldap://ad.example.com:389" }, wantError: "auth.ldap.url must be an ldaps://"},
		{name: "rejects HTTPS endpoint", change: func(c *LDAPConfig) { c.URL = "https://ad.example.com" }, wantError: "auth.ldap.url"},
		{name: "rejects missing host", change: func(c *LDAPConfig) { c.URL = "ldaps:///" }, wantError: "auth.ldap.url"},
		{name: "rejects URL credentials", change: func(c *LDAPConfig) { c.URL = "ldaps://user:secret@ad.example.com" }, wantError: "auth.ldap.url"},
		{name: "rejects URL path", change: func(c *LDAPConfig) { c.URL += "/dc=example" }, wantError: "auth.ldap.url"},
		{name: "rejects URL query", change: func(c *LDAPConfig) { c.URL += "?query" }, wantError: "auth.ldap.url"},
		{name: "rejects empty query", change: func(c *LDAPConfig) { c.URL += "?" }, wantError: "auth.ldap.url"},
		{name: "rejects URL fragment", change: func(c *LDAPConfig) { c.URL += "#fragment" }, wantError: "auth.ldap.url"},
		{name: "rejects empty fragment", change: func(c *LDAPConfig) { c.URL += "#" }, wantError: "auth.ldap.url"},
		{name: "rejects zero port", change: func(c *LDAPConfig) { c.URL = "ldaps://ad.example.com:0" }, wantError: "auth.ldap.url"},
		{name: "rejects oversized port", change: func(c *LDAPConfig) { c.URL = "ldaps://ad.example.com:65536" }, wantError: "auth.ldap.url"},
		{name: "rejects malformed port", change: func(c *LDAPConfig) { c.URL = "ldaps://ad.example.com:abc" }, wantError: "auth.ldap.url"},
		{name: "rejects invalid CA encoding", change: func(c *LDAPConfig) { c.CACert = "not base64" }, wantError: "auth.ldap.caCert"},
		{name: "rejects invalid CA PEM", change: func(c *LDAPConfig) { c.CACert = base64.StdEncoding.EncodeToString([]byte("not PEM")) }, wantError: "auth.ldap.caCert"},
		{name: "rejects missing bind DN", change: func(c *LDAPConfig) { c.BindDN = "" }, wantError: "auth.ldap.bindDN"},
		{name: "rejects malformed bind DN", change: func(c *LDAPConfig) { c.BindDN = "invalid" }, wantError: "auth.ldap.bindDN"},
		{name: "rejects missing search password", change: func(c *LDAPConfig) { c.BindPassword = "" }, wantError: "auth.ldap.bindPassword"},
		{name: "rejects missing user base", change: func(c *LDAPConfig) { c.UserBaseDN = "" }, wantError: "auth.ldap.userBaseDN"},
		{name: "rejects malformed user base", change: func(c *LDAPConfig) { c.UserBaseDN = "invalid" }, wantError: "auth.ldap.userBaseDN"},
		{name: "rejects missing group base", change: func(c *LDAPConfig) { c.GroupBaseDN = "" }, wantError: "auth.ldap.groupBaseDN"},
		{name: "rejects malformed group base", change: func(c *LDAPConfig) { c.GroupBaseDN = "invalid" }, wantError: "auth.ldap.groupBaseDN"},
		{name: "rejects missing username placeholder", change: func(c *LDAPConfig) { c.UserFilter = "(cn=alice)" }, wantError: "auth.ldap.userFilter"},
		{name: "rejects repeated username placeholder", change: func(c *LDAPConfig) { c.UserFilter = "(cn={username}{username})" }, wantError: "auth.ldap.userFilter"},
		{name: "rejects malformed user filter", change: func(c *LDAPConfig) { c.UserFilter = "(cn={username}" }, wantError: "auth.ldap.userFilter"},
		{name: "rejects missing group placeholder", change: func(c *LDAPConfig) { c.GroupFilter = "(cn=team)" }, wantError: "auth.ldap.groupFilter"},
		{name: "rejects repeated group placeholder", change: func(c *LDAPConfig) { c.GroupFilter = "(member={userDN}{userDN})" }, wantError: "auth.ldap.groupFilter"},
		{name: "rejects malformed group filter", change: func(c *LDAPConfig) { c.GroupFilter = "(member={userDN}" }, wantError: "auth.ldap.groupFilter"},
		{name: "rejects empty username attribute", change: func(c *LDAPConfig) { c.UsernameAttribute = "" }, wantError: "auth.ldap.usernameAttribute"},
		{name: "rejects malformed username attribute", change: func(c *LDAPConfig) { c.UsernameAttribute = "cn)(uid=*" }, wantError: "auth.ldap.usernameAttribute"},
		{name: "rejects malformed group attribute", change: func(c *LDAPConfig) { c.GroupNameAttribute = "cn;" }, wantError: "auth.ldap.groupNameAttribute"},
		{name: "accepts numeric attribute OID", change: func(c *LDAPConfig) { c.UsernameAttribute = "0.9.2342.19200300.100.1.1" }},
		{name: "accepts attribute option", change: func(c *LDAPConfig) { c.GroupNameAttribute = "cn;lang-en" }},
		{name: "rejects zero login timeout", change: func(c *LDAPConfig) { c.TimeoutSeconds = 0 }, wantError: "auth.ldap.timeoutSeconds"},
		{name: "rejects negative token TTL", change: func(c *LDAPConfig) { c.TokenTTLSeconds = -1 }, wantError: "auth.ldap.tokenTTLSeconds"},
		{name: "rejects blank issuer", change: func(c *LDAPConfig) { c.Issuer = " " }, wantError: "auth.ldap.issuer"},
		{name: "rejects missing audience", change: func(c *LDAPConfig) { c.Audience = "" }, wantError: "auth.ldap.audience"},
		{name: "rejects missing signing key ID", change: func(c *LDAPConfig) { c.SigningKeyID = "" }, wantError: "auth.ldap.signingKeyID"},
		{name: "rejects uppercase signing key ID", change: func(c *LDAPConfig) { c.SigningKeyID = "KEY-1" }, wantError: "auth.ldap.signingKeyID"},
		{name: "rejects missing private key", change: func(c *LDAPConfig) { c.SigningPrivateKey = "" }, wantError: "auth.ldap.signingPrivateKey"},
		{name: "rejects public key used for signing", change: func(c *LDAPConfig) { c.SigningPrivateKey = publicPEM }, wantError: "auth.ldap.signingPrivateKey"},
		{name: "rejects malformed private key", change: func(c *LDAPConfig) { c.SigningPrivateKey = "invalid" }, wantError: "auth.ldap.signingPrivateKey"},
		{name: "rejects non-Ed25519 signing key", change: func(c *LDAPConfig) { c.SigningPrivateKey = otherPrivatePEM }, wantError: "auth.ldap.signingPrivateKey"},
		{name: "rejects mismatched active key", change: func(c *LDAPConfig) { c.VerificationKeys[c.SigningKeyID] = publicPEM }, wantError: "conflicts with the active signing key ID"},
		{name: "rejects uppercase verification key ID", change: func(c *LDAPConfig) { c.VerificationKeys["KEY-2"] = publicPEM }, wantError: "verificationKeys IDs"},
		{name: "rejects malformed verification key", change: func(c *LDAPConfig) { c.VerificationKeys["key-2"] = "invalid" }, wantError: "verificationKeys must contain"},
		{name: "rejects private verification key", change: func(c *LDAPConfig) { c.VerificationKeys["key-2"] = privatePEM }, wantError: "verificationKeys must contain"},
		{name: "rejects non-Ed25519 verification key", change: func(c *LDAPConfig) { c.VerificationKeys["key-2"] = otherPublicPEM }, wantError: "verificationKeys must contain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := passthroughConfig()
			cfg.Clusters[0].Credential.Inline.Token = "sa-token"
			cfg.Auth.Mode = AuthModeLDAP
			cfg.Auth.LDAP = validLDAPConfig(t)
			tc.change(&cfg.Auth.LDAP)
			err := Validate(cfg)
			if tc.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestValidateLDAPModeIsolation(t *testing.T) {
	for _, mode := range []string{AuthModePassthrough, AuthModeOpenShift} {
		t.Run(mode, func(t *testing.T) {
			cfg := passthroughConfig()
			cfg.Clusters[0].Credential.Inline.Token = "sa-token"
			cfg.Auth.Mode = mode
			cfg.Auth.OpenShift = validOpenShiftConfig()
			cfg.Auth.LDAP = LDAPConfig{URL: "invalid", SigningPrivateKey: "invalid"}
			require.NoError(t, Validate(cfg))
		})
	}
}

func TestValidateLDAPClusterCredentials(t *testing.T) {
	for _, credential := range []CredentialConfig{
		{KubeconfigPath: "/etc/kubeconfig"},
		validInlineCredential(),
	} {
		cfg := passthroughConfig()
		cfg.Auth.Mode = AuthModeLDAP
		cfg.Auth.LDAP = validLDAPConfig(t)
		cfg.Clusters[0].Credential = credential
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "credential.inline.token is required when auth.mode is 'ldap'")
	}
}

func TestValidateLDAPRedactsSecrets(t *testing.T) {
	cfg := passthroughConfig()
	cfg.Clusters[0].Credential.Inline.Token = "sa-token"
	cfg.Auth.Mode = AuthModeLDAP
	cfg.Auth.LDAP = validLDAPConfig(t)
	cfg.Auth.LDAP.URL = "ldaps://search:secret-url-password@ad.example.com"
	cfg.Auth.LDAP.SigningPrivateKey = "secret-invalid-private-key"
	cfg.Auth.LDAP.VerificationKeys["key-2"] = "secret-invalid-verification-key"
	err := Validate(cfg)
	require.Error(t, err)
	for _, secret := range []string{
		"secret-url-password", cfg.Auth.LDAP.SigningPrivateKey, cfg.Auth.LDAP.BindPassword,
		"secret-invalid-verification-key",
	} {
		assert.NotContains(t, err.Error(), secret)
	}
}

func validLDAPConfig(t *testing.T) LDAPConfig {
	t.Helper()
	privatePEM, publicPEM := ldapSigningKeys(t)
	return LDAPConfig{
		URL: "ldaps://ad.example.com:636", BindDN: "CN=search,DC=example,DC=com", BindPassword: "search-password",
		UserBaseDN: "OU=users,DC=example,DC=com", UserFilter: "(sAMAccountName={username})", UsernameAttribute: "sAMAccountName",
		GroupBaseDN: "OU=groups,DC=example,DC=com", GroupFilter: "(&(objectClass=group)(member={userDN}))", GroupNameAttribute: "sAMAccountName",
		TimeoutSeconds: 10, TokenTTLSeconds: 900, Issuer: "capp-backend", Audience: "capp-api",
		SigningKeyID: "key-1", SigningPrivateKey: privatePEM, VerificationKeys: map[string]string{"key-1": publicPEM},
	}
}

func ldapSigningKeys(t *testing.T) (privatePEM, publicPEM string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
}

package root

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/resource"
)

func TestMaybeRefreshLDAP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offset  time.Duration
		unknown bool
		expired bool
	}{
		{name: "just before expiry", offset: time.Nanosecond},
		{name: "at expiry", expired: true},
		{name: "after expiry", offset: -time.Nanosecond, expired: true},
		{name: "unknown expiry", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := config.Context{
					AuthMode: "ldap", Token: "access", RefreshToken: "must-not-refresh",
					Server: "https://unused",
				}
				if !tc.unknown {
					ctx.TokenExpiresAt = time.Now().Add(tc.offset)
				}
				token, err := maybeRefresh(&cobra.Command{}, &config.Config{}, "", &ctx, false)
				if tc.expired {
					require.EqualError(t, err, "LDAP access token expired; run cappctl login again")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, "access", token)
			})
		})
	}
}

func TestNewLDAPContextExpiry(t *testing.T) {
	for _, selection := range []string{"active", "named"} {
		t.Run(selection, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("expired LDAP token caused a request to %s", r.URL.Path)
			}))
			t.Cleanup(srv.Close)
			cfg := &config.Config{CurrentContext: "expired", Contexts: []config.Context{
				{Name: "expired", Server: srv.URL, AuthMode: "ldap", Token: "expired-token", TokenExpiresAt: time.Now().Add(-time.Hour).UTC()},
				{Name: "valid", Server: srv.URL, AuthMode: "ldap", Token: "valid-token", TokenExpiresAt: time.Now().Add(time.Hour).UTC()},
			}}
			var flags []string
			if selection == "named" {
				cfg.CurrentContext = "valid"
				flags = []string{"--context", "expired"}
			}
			cmd, state := newResourceCommand(t, cfg, flags...)
			resourceCmd, _, err := cmd.Find([]string{"get", "probe"})
			require.NoError(t, err)
			resourceCmd.RunE = func(_ *cobra.Command, _ []string) error {
				t.Error("resource handler ran with an expired token")
				return nil
			}

			require.EqualError(t, cmd.ExecuteContext(t.Context()), "LDAP access token expired; run cappctl login again")
			saved, err := config.Load(state.CfgPath)
			require.NoError(t, err)
			assert.Equal(t, cfg, saved)
		})
	}
}

func TestNewTokenOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, contextName, flagToken, envToken, want string
	}{
		{name: "flag overrides environment and active context", flagToken: "flag-token", envToken: "env-token", want: "flag-token"},
		{name: "environment overrides active context", envToken: "env-token", want: "env-token"},
		{name: "flag overrides environment and named context", contextName: "expired", flagToken: "flag-token", envToken: "env-token", want: "flag-token"},
		{name: "environment overrides named context", contextName: "expired", envToken: "env-token", want: "env-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/resource", r.URL.Path)
				assert.Equal(t, "Bearer "+tc.want, r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)
			cfg := &config.Config{CurrentContext: "expired", Contexts: []config.Context{
				{Name: "expired", Server: srv.URL, AuthMode: "ldap", Token: "expired-token", RefreshToken: "must-not-refresh", TokenExpiresAt: time.Now().Add(-time.Hour)},
			}}
			var flags []string
			if tc.contextName != "" {
				cfg.CurrentContext = "other"
				flags = append(flags, "--context", tc.contextName)
			}
			if tc.flagToken != "" {
				flags = append(flags, "--token", tc.flagToken)
			}
			cmd, _ := newResourceCommand(t, cfg, flags...)
			t.Setenv("CAPP_TOKEN", tc.envToken)

			require.NoError(t, cmd.ExecuteContext(t.Context()))
		})
	}
}

func TestNewContextToken(t *testing.T) {
	for _, tc := range []struct {
		name, mode, refreshToken string
		expiresIn                time.Duration
	}{
		{name: "LDAP within refresh window ignores refresh metadata", mode: "ldap", refreshToken: "must-not-refresh", expiresIn: 20 * time.Second},
		{name: "passthrough ignores expiry metadata", mode: "passthrough", expiresIn: -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/resource", r.URL.Path)
				assert.Equal(t, "Bearer context-token", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)
			cfg := &config.Config{CurrentContext: "target", Contexts: []config.Context{{
				Name: "target", Server: srv.URL, AuthMode: tc.mode, Token: "context-token",
				RefreshToken: tc.refreshToken, TokenExpiresAt: time.Now().Add(tc.expiresIn),
			}}}
			cmd, _ := newResourceCommand(t, cfg)

			require.NoError(t, cmd.ExecuteContext(t.Context()))
		})
	}
}

func TestNewOpenShiftRefresh(t *testing.T) {
	pair := client.TokenPair{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			assert.Equal(t, http.MethodPost, r.Method)
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				return
			}
			assert.Equal(t, map[string]string{"refreshToken": "old-refresh"}, body)
			assert.NoError(t, json.NewEncoder(w).Encode(pair))
		case "/resource":
			assert.Equal(t, "Bearer new-access", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{CurrentContext: "target", Contexts: []config.Context{{
		Name: "target", Server: srv.URL, AuthMode: "openshift", Token: "old-access",
		RefreshToken: "old-refresh", TokenExpiresAt: time.Now().Add(20 * time.Second),
	}}}
	cmd, state := newResourceCommand(t, cfg)

	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.Equal(t, []string{"/api/v1/auth/refresh", "/resource"}, paths)
	saved, err := config.Load(state.CfgPath)
	require.NoError(t, err)
	ctx, ok := saved.GetContext("target")
	require.True(t, ok)
	assert.Equal(t, pair.AccessToken, ctx.Token)
	assert.Equal(t, pair.RefreshToken, ctx.RefreshToken)
	assert.Equal(t, pair.ExpiresAt, ctx.TokenExpiresAt)
}

func newResourceCommand(t *testing.T, cfg *config.Config, flags ...string) (*cobra.Command, *State) {
	t.Helper()
	for _, name := range []string{"CAPP_SERVER", "CAPP_TOKEN", "CAPP_CLUSTER", "CAPP_NAMESPACE"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, config.Save(path, cfg))
	state := &State{}
	cmd := New(state, resource.NewRegistry())
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors = true
	get, _, err := cmd.Find([]string{"get"})
	require.NoError(t, err)
	get.AddCommand(&cobra.Command{
		Use: "probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return state.Client.Get(cmd.Context(), "/resource", nil)
		},
	})
	cmd.SetArgs(append([]string{"--config", path, "get", "probe"}, flags...))
	return cmd, state
}

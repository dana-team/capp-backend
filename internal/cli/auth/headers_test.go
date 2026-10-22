package auth

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/resource"
	"github.com/dana-team/capp-backend/internal/cli/root"
)

func TestMCPHeadersCommand(t *testing.T) {
	state := &root.State{Token: "fresh-token"}

	cmd := NewMCPHeadersCommand(state)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)

	require.NoError(t, cmd.Execute())

	var headers map[string]string
	require.NoError(t, json.Unmarshal(buf.Bytes(), &headers))
	require.Equal(t, "Bearer fresh-token", headers["Authorization"])
}

func TestNewMCPHeadersCommandContextToken(t *testing.T) {
	for _, tc := range []struct {
		name, mode, contextName, flagToken, envToken, want string
		expiresIn                                          time.Duration
	}{
		{name: "expired active LDAP context", mode: "ldap", expiresIn: -time.Hour},
		{name: "expired named LDAP context", mode: "ldap", contextName: "target", expiresIn: -time.Hour},
		{name: "LDAP within refresh window", mode: "ldap", expiresIn: 20 * time.Second, want: "context-token"},
		{name: "flag overrides expired active context", mode: "ldap", expiresIn: -time.Hour, flagToken: "flag-token", envToken: "env-token", want: "flag-token"},
		{name: "environment overrides expired named context", mode: "ldap", contextName: "target", expiresIn: -time.Hour, envToken: "env-token", want: "env-token"},
		{name: "passthrough ignores expiry metadata", mode: "passthrough", expiresIn: -time.Hour, want: "context-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{CurrentContext: "target", Contexts: []config.Context{
				{Name: "target", Server: "https://unused", AuthMode: tc.mode, Token: "context-token", TokenExpiresAt: time.Now().Add(tc.expiresIn)},
				{Name: "other", Server: "https://unused", AuthMode: "ldap", Token: "other-token", TokenExpiresAt: time.Now().Add(time.Hour)},
			}}
			var flags []string
			if tc.contextName != "" {
				cfg.CurrentContext = "other"
				flags = append(flags, "--context", tc.contextName)
			}
			if tc.flagToken != "" {
				flags = append(flags, "--token", tc.flagToken)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, config.Save(path, cfg))
			cmd, out := newRootMCPHeadersCommand(t, path, flags...)
			t.Setenv("CAPP_TOKEN", tc.envToken)

			err := cmd.ExecuteContext(t.Context())
			if tc.want == "" {
				require.EqualError(t, err, "LDAP access token expired; run cappctl login again")
				require.Empty(t, out.String(), "expired contexts must not emit headers")
				return
			}
			require.NoError(t, err)
			var headers map[string]string
			require.NoError(t, json.Unmarshal(out.Bytes(), &headers))
			require.Equal(t, map[string]string{"Authorization": "Bearer " + tc.want}, headers)
		})
	}
}

func TestNewMCPHeadersCommandImportedTokenExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		expiresAt := time.Now().Add(time.Minute)
		token := loginToken(t, jwt.MapClaims{"exp": expiresAt.Unix()})
		login, state, _ := newLoginCommand(t, "https://unused", "--auth-mode", "ldap", "--token", token)
		require.NoError(t, login.ExecuteContext(t.Context()))
		cmd, out := newRootMCPHeadersCommand(t, state.CfgPath)

		time.Sleep(time.Minute)
		require.EqualError(t, cmd.ExecuteContext(t.Context()), "LDAP access token expired; run cappctl login again")
		require.Empty(t, out.String(), "imported tokens must not emit headers at expiry")
	})
}

func newRootMCPHeadersCommand(t *testing.T, path string, flags ...string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	for _, name := range []string{"CAPP_SERVER", "CAPP_TOKEN", "CAPP_CLUSTER", "CAPP_NAMESPACE"} {
		t.Setenv(name, "")
	}
	state := &root.State{}
	cmd := root.New(state, resource.NewRegistry())
	cmd.AddCommand(NewMCPHeadersCommand(state))
	cmd.SilenceErrors = true
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(forbiddenLoginInput{t: t})
	cmd.SetArgs(append([]string{"--config", path, "mcp-headers"}, flags...))
	return cmd, out
}

package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/resource"
	"github.com/dana-team/capp-backend/internal/cli/root"
)

func TestNewLoginCommandLDAP(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{name: "no newline", input: "sëcret🔑", want: "sëcret🔑"},
		{name: "LF", input: "secret\n", want: "secret"},
		{name: "CRLF", input: "secret\r\n", want: "secret"},
		{name: "preserve whitespace", input: " \tsecret\nline\n\n", want: " \tsecret\nline\n"},
		{name: "preserve carriage return", input: "secret\r", want: "secret\r"},
		{name: "preserve NUL", input: "a\x00b\n", want: "a\x00b"},
		{name: "raw limit", input: strings.Repeat("é", 8192), want: strings.Repeat("é", 8192)},
		{name: "raw limit with LF", input: strings.Repeat("a", 16383) + "\n", want: strings.Repeat("a", 16383)},
		{name: "raw limit with CRLF", input: strings.Repeat("a", 16382) + "\r\n", want: strings.Repeat("a", 16382)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expiresAt := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/auth/mode" {
					assert.Equal(t, http.MethodGet, r.Method)
					json.NewEncoder(w).Encode(map[string]string{"mode": "ldap"}) //nolint:errcheck
					return
				}
				assert.Equal(t, "/api/v1/auth/login", r.URL.Path)
				assert.Equal(t, http.MethodPost, r.Method)
				var credentials map[string]string
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&credentials)) {
					return
				}
				assert.Equal(t, map[string]string{"username": "automation", "password": tc.want}, credentials)
				json.NewEncoder(w).Encode(client.TokenPair{ //nolint:errcheck
					AccessToken: "ldap-access", ExpiresAt: expiresAt, RefreshToken: "must-not-persist",
				})
			}))
			t.Cleanup(srv.Close)
			trustLoginServer(t, srv)
			cmd, state, out := newLoginCommand(t, srv.URL, "--username", "automation", "--password-stdin")
			cmd.SetIn(strings.NewReader(tc.input))

			require.NoError(t, cmd.ExecuteContext(t.Context()))

			ctx := savedLoginContext(t, state)
			assert.Equal(t, config.Context{
				Name: "target", Server: srv.URL, AuthMode: "ldap", Token: "ldap-access",
				TokenExpiresAt: expiresAt, Cluster: "east", Namespace: "apps",
			}, ctx)
			info, err := os.Stat(state.CfgPath)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
			data, err := os.ReadFile(state.CfgPath)
			require.NoError(t, err)
			assert.NotContains(t, string(data), "password")
			assert.NotContains(t, string(data), "refresh-token")
			assert.NotContains(t, out.String(), "Password:")
			assert.NotContains(t, out.String(), "Opening browser")
		})
	}
}

func TestNewLoginCommandLDAPInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		input io.Reader
		want  string
	}{
		{name: "empty", input: strings.NewReader(""), want: "must not be empty"},
		{name: "only LF", input: strings.NewReader("\n"), want: "must not be empty"},
		{name: "only CRLF", input: strings.NewReader("\r\n"), want: "must not be empty"},
		{name: "invalid UTF-8", input: strings.NewReader("secret\xff\n"), want: "must be valid UTF-8"},
		{name: "overflow", input: strings.NewReader(strings.Repeat("a", 16385)), want: "16 KiB limit"},
		{name: "LF counts toward limit", input: strings.NewReader(strings.Repeat("a", 16384) + "\n"), want: "16 KiB limit"},
		{name: "read failure", input: iotest.ErrReader(errors.New("input failed")), want: "reading password from stdin: input failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Error("invalid input sent a credential request")
			}))
			t.Cleanup(srv.Close)
			trustLoginServer(t, srv)
			cmd, state, _ := newLoginCommand(t, srv.URL, "--auth-mode", "ldap", "--username", "automation", "--password-stdin")
			cmd.SetIn(tc.input)
			requireLoginFailure(t, cmd, state, tc.want)
		})
	}
}

func TestNewLoginCommandLDAPInvalidFlags(t *testing.T) {
	cases := []struct {
		name, server, want string
		flags              []string
	}{
		{name: "HTTP", server: "http://unused", flags: []string{"--username", "user", "--password-stdin"}, want: "certificate-verified HTTPS"},
		{name: "invalid URL", server: "https://%", want: "certificate-verified HTTPS"},
		{name: "missing host", server: "https:///path", want: "certificate-verified HTTPS"},
		{name: "insecure", flags: []string{"--insecure", "--username", "user", "--password-stdin"}, want: "--insecure is not allowed"},
		{name: "missing username", flags: []string{"--password-stdin"}, want: "--username is required"},
		{name: "empty username", flags: []string{"--username=", "--password-stdin"}, want: "--username is required"},
		{name: "missing stdin flag", flags: []string{"--username", "user"}, want: "--password-stdin is required"},
		{name: "disabled stdin", flags: []string{"--username", "user", "--password-stdin=false"}, want: "--password-stdin is required"},
		{name: "password argument", flags: []string{"--username", "user", "--password", "secret"}, want: "--password is not allowed"},
		{name: "empty password argument", flags: []string{"--username", "user", "--password=", "--password-stdin"}, want: "--password is not allowed"},
		{name: "token and username", flags: []string{"--token", "token", "--username="}, want: "--token cannot be combined"},
		{name: "token and password", flags: []string{"--token", "token", "--password="}, want: "--token cannot be combined"},
		{name: "token and stdin", flags: []string{"--token", "token", "--password-stdin"}, want: "--token cannot be combined"},
		{name: "token and disabled stdin", flags: []string{"--token", "token", "--password-stdin=false"}, want: "--token cannot be combined"},
		{name: "empty token and credentials", flags: []string{"--token=", "--username", "user", "--password-stdin"}, want: "--token cannot be combined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.server
			if server == "" {
				server = "https://unused"
			}
			flags := append([]string{"--auth-mode", "ldap"}, tc.flags...)
			cmd, state, _ := newLoginCommand(t, server, flags...)
			requireLoginFailure(t, cmd, state, tc.want)
		})
	}
}

func TestNewLoginCommandPasswordStdinOtherModes(t *testing.T) {
	for _, detected := range []bool{false, true} {
		t.Run(fmt.Sprintf("openshift detected=%t", detected), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.True(t, detected, "explicit mode must not make requests")
				assert.Equal(t, "/api/v1/auth/mode", r.URL.Path)
				json.NewEncoder(w).Encode(map[string]string{"mode": "openshift"}) //nolint:errcheck
			}))
			t.Cleanup(srv.Close)
			flags := []string{"--password-stdin"}
			if !detected {
				flags = append(flags, "--auth-mode", "openshift")
			}
			cmd, state, _ := newLoginCommand(t, srv.URL, flags...)
			requireLoginFailure(t, cmd, state, "--password-stdin is only supported in ldap mode")
		})
	}
}

func TestNewLoginCommandLDAPUntrustedTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("credentials reached a server with an untrusted certificate")
	}))
	t.Cleanup(srv.Close)
	cmd, state, _ := newLoginCommand(t, srv.URL, "--auth-mode", "ldap", "--username", "user", "--password-stdin")
	cmd.SetIn(strings.NewReader("secret"))
	requireLoginFailure(t, cmd, state, "certificate")
}

func TestNewLoginCommandLDAPRedirects(t *testing.T) {
	// Cover redirects that rewrite POST to GET and those that replay its body.
	for _, status := range []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect} {
		for _, destination := range []string{"same origin", "cross origin", "HTTP downgrade"} {
			t.Run(fmt.Sprintf("%d %s", status, destination), func(t *testing.T) {
				handler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
					t.Error("followed a credential redirect")
				})
				target := httptest.NewUnstartedServer(handler)
				if destination == "HTTP downgrade" {
					target.Start()
				} else {
					target.StartTLS()
				}
				t.Cleanup(target.Close)
				location := target.URL + "/redirected"
				if destination == "same origin" {
					location = "/redirected"
				}
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/auth/login" {
						t.Error("followed a same-origin credential redirect")
						return
					}
					http.Redirect(w, r, location, status)
				}))
				t.Cleanup(srv.Close)
				trustLoginServer(t, srv)
				cmd, state, _ := newLoginCommand(t, srv.URL, "--auth-mode", "ldap", "--username", "user", "--password-stdin")
				cmd.SetIn(strings.NewReader("secret"))
				requireLoginFailure(t, cmd, state, "redirects are not allowed")
			})
		}
	}
}

func TestNewLoginCommandLDAPTokenImport(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	// Deliberately not a backend-issued token: import reads expiry, not identity
	// or signature validity. The backend must authenticate subsequent requests.
	token := loginToken(t, jwt.MapClaims{"exp": expiresAt.Unix(), "iss": "untrusted", "token_use": "not-access"})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("explicit token import must not make API requests")
	}))
	t.Cleanup(srv.Close)
	cmd, state, _ := newLoginCommand(t, srv.URL, "--auth-mode", "ldap", "--token", token)
	old := &config.Config{CurrentContext: "other", Contexts: []config.Context{
		{Name: "target", AuthMode: "openshift", Token: "old", RefreshToken: "old-refresh", TokenExpiresAt: time.Now().Add(-time.Hour)},
		{Name: "other", Token: "untouched"},
	}}
	require.NoError(t, config.Save(state.CfgPath, old))

	require.NoError(t, cmd.ExecuteContext(t.Context()))

	ctx := savedLoginContext(t, state)
	assert.Equal(t, "ldap", ctx.AuthMode)
	assert.Equal(t, token, ctx.Token)
	assert.Equal(t, expiresAt, ctx.TokenExpiresAt)
	assert.Empty(t, ctx.RefreshToken)
	require.Len(t, state.Cfg.Contexts, 2)
	assert.Equal(t, old.Contexts[1], state.Cfg.Contexts[1])
}

func TestNewLoginCommandLDAPInvalidToken(t *testing.T) {
	cases := []struct {
		name, token, want string
	}{
		{name: "empty", want: "invalid LDAP access token"},
		{name: "malformed", token: "not-a-jwt", want: "invalid LDAP access token"},
		{name: "invalid signature encoding", token: "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjIwMDAwMDAwMDB9.!", want: "invalid LDAP access token"},
		{name: "missing expiry", token: loginToken(t, jwt.MapClaims{}), want: "positive numeric exp"},
		{name: "string expiry", token: loginToken(t, jwt.MapClaims{"exp": "2000000000"}), want: "positive numeric exp"},
		{name: "null expiry", token: loginToken(t, jwt.MapClaims{"exp": nil}), want: "positive numeric exp"},
		{name: "boolean expiry", token: loginToken(t, jwt.MapClaims{"exp": true}), want: "positive numeric exp"},
		{name: "zero expiry", token: loginToken(t, jwt.MapClaims{"exp": 0}), want: "positive numeric exp"},
		{name: "negative expiry", token: loginToken(t, jwt.MapClaims{"exp": -1}), want: "positive numeric exp"},
		{name: "overflow expiry", token: loginToken(t, jwt.MapClaims{"exp": 1e100}), want: "positive numeric exp"},
		{name: "unstorable expiry", token: loginToken(t, jwt.MapClaims{"exp": 253402300800}), want: "expiry cannot be stored"},
		{name: "expired", token: loginToken(t, jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}), want: "LDAP access token expired; run cappctl login again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, state, _ := newLoginCommand(t, "https://unused", "--auth-mode", "ldap", "--token", tc.token)
			requireLoginFailure(t, cmd, state, tc.want)
		})
	}
}

func TestNewLoginCommandLDAPFailurePreservesContext(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": client.APIError{ //nolint:errcheck
			Code: "UNAUTHORIZED", Message: "invalid credentials", Status: http.StatusUnauthorized,
		}})
	}))
	t.Cleanup(srv.Close)
	trustLoginServer(t, srv)
	cmd, state, _ := newLoginCommand(t, srv.URL, "--auth-mode", "ldap", "--username", "user", "--password-stdin")
	cmd.SetIn(strings.NewReader("rejected"))
	old := &config.Config{CurrentContext: "target", Contexts: []config.Context{{Name: "target", AuthMode: "openshift", Token: "old", RefreshToken: "refresh"}}}
	require.NoError(t, config.Save(state.CfgPath, old))

	err := cmd.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "invalid credentials")
	var apiErr *client.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "UNAUTHORIZED", apiErr.Code)
	saved, err := config.Load(state.CfgPath)
	require.NoError(t, err)
	assert.Equal(t, old, saved)
	assert.Equal(t, old, state.Cfg)
}

func TestNewLoginCommandLDAPInvalidResponse(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	validBody := fmt.Sprintf(`{"accessToken":"new-token","expiresAt":%q}`, expiresAt)
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "empty body", status: http.StatusOK, want: "requires an access token and expiry"},
		{name: "null response", status: http.StatusOK, body: `null`, want: "requires an access token and expiry"},
		{name: "no content", status: http.StatusNoContent, want: "requires an access token and expiry"},
		{name: "redirect without location", status: http.StatusTemporaryRedirect, body: validBody, want: "unexpected HTTP status 307"},
		{name: "missing token", status: http.StatusOK, body: fmt.Sprintf(`{"expiresAt":%q}`, expiresAt), want: "requires an access token and expiry"},
		{name: "blank token", status: http.StatusOK, body: fmt.Sprintf(`{"accessToken":" \t","expiresAt":%q}`, expiresAt), want: "requires an access token and expiry"},
		{name: "missing expiry", status: http.StatusOK, body: `{"accessToken":"new-token"}`, want: "requires an access token and expiry"},
		{name: "zero expiry", status: http.StatusOK, body: `{"accessToken":"new-token","expiresAt":"0001-01-01T00:00:00Z"}`, want: "requires an access token and expiry"},
		{name: "expired token", status: http.StatusOK, body: `{"accessToken":"new-token","expiresAt":"2000-01-01T00:00:00Z"}`, want: "LDAP access token expired; run cappctl login again"},
		{name: "invalid expiry", status: http.StatusOK, body: `{"accessToken":"new-token","expiresAt":"not-a-timestamp"}`, want: "decoding response"},
		{name: "unstorable expiry", status: http.StatusOK, body: `{"accessToken":"new-token","expiresAt":"9000-01-01T00:00:00+24:00"}`, want: "expiry cannot be stored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body) //nolint:errcheck
			}))
			t.Cleanup(srv.Close)
			trustLoginServer(t, srv)
			cmd, state, out := newLoginCommand(t, srv.URL, "--auth-mode", "ldap", "--username", "user", "--password-stdin")
			cmd.SetIn(strings.NewReader("secret"))
			old := &config.Config{CurrentContext: "other", Contexts: []config.Context{
				{Name: "target", AuthMode: "openshift", Token: "working-token", RefreshToken: "working-refresh"},
				{Name: "other", Token: "untouched"},
			}}
			require.NoError(t, config.Save(state.CfgPath, old))
			before, err := os.ReadFile(state.CfgPath)
			require.NoError(t, err)

			require.ErrorContains(t, cmd.ExecuteContext(t.Context()), tc.want)

			after, err := os.ReadFile(state.CfgPath)
			require.NoError(t, err)
			assert.Equal(t, before, after, "failed login must not write the config")
			assert.Equal(t, old, state.Cfg)
			assert.NotContains(t, out.String(), "Logged in.")
		})
	}
}

func TestNewLoginCommandSaveFailurePreservesContext(t *testing.T) {
	token := loginToken(t, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})
	cmd, state, _ := newLoginCommand(t, "https://unused", "--auth-mode", "ldap", "--token", token)
	old := &config.Config{CurrentContext: "target", Contexts: []config.Context{{Name: "target", Token: "old", RefreshToken: "refresh"}}}
	require.NoError(t, config.Save(state.CfgPath, old))
	loginCmd, _, err := cmd.Find([]string{"login"})
	require.NoError(t, err)
	loginCmd.PreRun = func(_ *cobra.Command, _ []string) { state.CfgPath = t.TempDir() }

	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "saving config")
	assert.Equal(t, old, state.Cfg)
}

func TestNewLoginCommandDirectTokenCompatibility(t *testing.T) {
	for _, mode := range []string{"openshift", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			cmd, state, _ := newLoginCommand(t, "http://unused", "--auth-mode", mode, "--token", "opaque-token", "--insecure")
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			ctx := savedLoginContext(t, state)
			assert.Equal(t, mode, ctx.AuthMode)
			assert.Equal(t, "opaque-token", ctx.Token)
			assert.True(t, ctx.TokenExpiresAt.IsZero())
		})
	}
}

func TestNewLoginCommandOpenShiftPassword(t *testing.T) {
	pair := client.TokenPair{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			http.Redirect(w, r, "/password-login", http.StatusTemporaryRedirect)
			return
		}
		assert.Equal(t, "/password-login", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		var credentials map[string]string
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&credentials)) {
			return
		}
		assert.Equal(t, map[string]string{"username": "user", "password": "secret"}, credentials)
		json.NewEncoder(w).Encode(pair) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	cmd, state, _ := newLoginCommand(t, srv.URL, "--auth-mode", "openshift", "--username", "user", "--password", "secret")
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	ctx := savedLoginContext(t, state)
	assert.Equal(t, pair.AccessToken, ctx.Token)
	assert.Equal(t, pair.RefreshToken, ctx.RefreshToken)
	assert.Equal(t, pair.ExpiresAt, ctx.TokenExpiresAt)
}

func TestNewLoginCommandOpenShiftBrowser(t *testing.T) {
	// Exercise the callback without launching a browser on the developer's machine.
	t.Setenv("PATH", t.TempDir())
	pair := client.TokenPair{AccessToken: "browser-access", RefreshToken: "browser-refresh", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/openshift/authorize":
			callback := r.URL.Query().Get("redirect_uri")
			resp, err := http.Get(callback + "?code=code&state=state")
			if !assert.NoError(t, err) {
				return
			}
			resp.Body.Close()                                                                                                    //nolint:errcheck
			json.NewEncoder(w).Encode(map[string]string{"authorizeUrl": "https://identity.example/authorize", "state": "state"}) //nolint:errcheck
		case "/api/v1/auth/openshift/callback":
			var body map[string]string
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
				return
			}
			assert.Equal(t, "code", body["code"])
			assert.Equal(t, "state", body["state"])
			assert.Equal(t, fmt.Sprintf("http://localhost:%d/callback", oauthCallbackPort), body["redirectUri"])
			json.NewEncoder(w).Encode(pair) //nolint:errcheck
		default:
			t.Errorf("unexpected browser login path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	cmd, state, out := newLoginCommand(t, srv.URL, "--auth-mode", "openshift")
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	ctx := savedLoginContext(t, state)
	assert.Equal(t, pair.AccessToken, ctx.Token)
	assert.Equal(t, pair.RefreshToken, ctx.RefreshToken)
	assert.Equal(t, pair.ExpiresAt, ctx.TokenExpiresAt)
	assert.Contains(t, out.String(), "Opening browser")
}

func TestNewLoginCommandHelp(t *testing.T) {
	cmd, _, out := newLoginCommand(t, "https://unused", "--help")
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	for _, text := range []string{"openshift|passthrough|ldap", "--password-stdin", "--username", "--token", "certificate-verified HTTPS", "16 KiB"} {
		assert.Contains(t, out.String(), text)
	}
}

func newLoginCommand(t *testing.T, server string, flags ...string) (*cobra.Command, *root.State, *bytes.Buffer) {
	t.Helper()
	state := &root.State{CfgPath: filepath.Join(t.TempDir(), "config.yaml")}
	cmd := root.New(state, resource.NewRegistry())
	cmd.SilenceErrors = true
	cmd.AddCommand(NewLoginCommand(state))
	cmd.SetArgs(append([]string{"login", "--config", state.CfgPath, "--server", server, "--context", "target", "--cluster", "east", "--namespace", "apps"}, flags...))
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(forbiddenLoginInput{t: t})
	return cmd, state, out
}

type forbiddenLoginInput struct{ t *testing.T }

func (r forbiddenLoginInput) Read([]byte) (int, error) {
	r.t.Error("login unexpectedly read stdin")
	return 0, errors.New("unexpected stdin read")
}

func trustLoginServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	transport, ok := srv.Client().Transport.(*http.Transport)
	require.True(t, ok)
	transport = transport.Clone()
	require.False(t, transport.TLSClientConfig.InsecureSkipVerify)
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = previous
		transport.CloseIdleConnections()
	})
}

func savedLoginContext(t *testing.T, state *root.State) config.Context {
	t.Helper()
	cfg, err := config.Load(state.CfgPath)
	require.NoError(t, err)
	assert.Equal(t, "target", cfg.CurrentContext)
	ctx, ok := cfg.GetContext("target")
	require.True(t, ok)
	return *ctx
}

func requireLoginFailure(t *testing.T, cmd *cobra.Command, state *root.State, want string) {
	t.Helper()
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), want)
	_, err := os.Stat(state.CfgPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, state.Cfg.Contexts)
}

func loginToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("untrusted-signing-key"))
	require.NoError(t, err)
	return token
}

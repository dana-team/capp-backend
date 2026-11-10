package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/clientversion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestNewWarnsOutdatedKnownClients checks the server wiring for every client
// in clientversion.Known: the warning reaches the client on every response,
// including requests the rate limiter rejects, and the outdated-client
// counter is exported on /metrics.
func TestNewWarnsOutdatedKnownClients(t *testing.T) {
	require.NotEmpty(t, clientversion.Known)

	cfg := &config.Config{Auth: config.AuthConfig{Mode: config.AuthModePassthrough}}
	cfg.Auth.RateLimit.Enabled = true
	cfg.Auth.RateLimit.RequestsPerSecond = 0.001
	cfg.Auth.RateLimit.Burst = 1
	cfg.Auth.RateLimit.MaxIPTracked = len(clientversion.Known) + 10
	cfg.Metrics.Enabled = true
	cfg.Metrics.Path = "/metrics"
	s, err := New(cfg, "v0.6.2", &testutil.MockAuthManager{}, &testutil.MockClusterManager{}, &resources.Registry{}, zap.NewNop())
	require.NoError(t, err)

	serve := func(ip, userAgent, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":1234"
		req.Header.Set("User-Agent", userAgent)
		w := httptest.NewRecorder()
		s.httpServer.Handler.ServeHTTP(w, req)
		return w
	}

	for i, client := range clientversion.Known {
		// Each client has its own IP, so its second request is rate limited.
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		for _, wantStatus := range []int{http.StatusOK, http.StatusTooManyRequests} {
			w := serve(ip, client.UserAgent("v0.5.2"), "/healthz")
			assert.Equal(t, wantStatus, w.Code, client.Name)
			assert.Equal(t, client.Warning("v0.5.2", "v0.6.2"), w.Header().Get(clientversion.WarningHeader), client.Name)
		}
	}

	metrics := serve("198.51.100.1", "", "/metrics")
	require.Equal(t, http.StatusOK, metrics.Code)
	for _, client := range clientversion.Known {
		assert.Contains(t, metrics.Body.String(),
			fmt.Sprintf(`capp_backend_outdated_client_requests_total{client=%q,version="v0.5"} `, client.Name))
	}
}

// TestNewLogsOutdatedClientUser checks, through the real middleware order,
// that the warning log names the user in openshift and ldap modes, for every
// client in clientversion.Known. Each mode's mock returns the credential that
// mode's AuthManager builds (see the Authenticate tests in internal/auth):
// openshift and ldap set ImpersonateUser, passthrough only has the token.
func TestNewLogsOutdatedClientUser(t *testing.T) {
	const token = "secret-bearer-token"

	for _, tc := range []struct {
		mode     string
		cred     auth.ClusterCredential
		wantUser string // empty means no user field
	}{
		{
			mode:     config.AuthModeOpenShift,
			cred:     auth.ClusterCredential{ImpersonateUser: "jane", ImpersonateGroups: []string{"developers"}},
			wantUser: "jane",
		},
		{
			mode:     config.AuthModeLDAP,
			cred:     auth.ClusterCredential{ImpersonateUser: "Alice", ImpersonateGroups: []string{"Developers"}},
			wantUser: "Alice",
		},
		{mode: config.AuthModePassthrough, cred: auth.ClusterCredential{BearerToken: token}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			authMgr := &testutil.MockAuthManager{
				AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
					return tc.cred, nil
				},
			}
			cfg := &config.Config{Auth: config.AuthConfig{Mode: tc.mode}}
			s, err := New(cfg, "v0.6.2", authMgr, &testutil.MockClusterManager{}, &resources.Registry{}, zap.New(core))
			require.NoError(t, err)

			for _, client := range clientversion.Known {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
				req.Header.Set("User-Agent", client.UserAgent("v0.5.2"))
				req.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				s.httpServer.Handler.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, client.Name)

				var warnings []map[string]any
				for _, entry := range logs.TakeAll() {
					if entry.Message == "outdated client" {
						warnings = append(warnings, entry.ContextMap())
					}
				}
				require.Len(t, warnings, 1, client.Name)
				fields := warnings[0]
				assert.Equal(t, client.Name, fields["client"])
				if tc.wantUser == "" {
					assert.NotContains(t, fields, "user")
				} else {
					assert.Equal(t, tc.wantUser, fields["user"])
				}
				for key, value := range fields {
					assert.NotContains(t, fmt.Sprint(value), token, "field %q leaks the bearer token", key)
				}
			}
		})
	}
}

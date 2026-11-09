package middleware_test

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/pkg/clientversion"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const outdatedMetric = "capp_backend_outdated_client_requests_total"

// serveClientVersion sends one GET /test with userAgent through handlers.
func serveClientVersion(t *testing.T, userAgent string, handlers ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(handlers...)
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("User-Agent", userAgent)
	engine.ServeHTTP(w, req)
	return w
}

// outdatedCounts returns the outdated-client counter keyed "client/version",
// read from the default registry that /metrics serves. The counter is global,
// so tests compare values before and after a request.
func outdatedCounts(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	counts := map[string]float64{}
	for _, family := range families {
		if family.GetName() != outdatedMetric {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			counts[labels["client"]+"/"+labels["version"]] = m.GetCounter().GetValue()
		}
	}
	return counts
}

// TestClientVersionWarnsOutdatedKnownClients runs every case for every client
// in clientversion.Known, so a newly listed client is covered automatically.
func TestClientVersionWarnsOutdatedKnownClients(t *testing.T) {
	require.NotEmpty(t, clientversion.Known)

	for _, client := range clientversion.Known {
		for _, tc := range []struct {
			name                         string
			serverVersion, clientVersion string
			// wantLabel is the metric's version label; empty means no warning.
			wantLabel string
		}{
			{name: "one minor behind", serverVersion: "v0.6.2", clientVersion: "v0.5.2", wantLabel: "v0.5"},
			{name: "several minors behind", serverVersion: "v0.6.2", clientVersion: "v0.3.9", wantLabel: "v0.3"},
			{name: "minors compared as numbers", serverVersion: "v0.12.1", clientVersion: "v0.9.0", wantLabel: "v0.9"},
			{name: "older major", serverVersion: "v1.2.0", clientVersion: "v0.9.4", wantLabel: "v0.x"},
			{name: "version without v prefix", serverVersion: "v0.6.2", clientVersion: "0.5.3", wantLabel: "v0.5"},
			{name: "older patch only", serverVersion: "v0.6.2", clientVersion: "v0.6.0"},
			{name: "same version", serverVersion: "v0.6.2", clientVersion: "v0.6.2"},
			{name: "newer client", serverVersion: "v0.6.2", clientVersion: "v0.7.0"},
			{name: "dev client", serverVersion: "v0.6.2", clientVersion: "dev"},
			{name: "dev server", serverVersion: "dev", clientVersion: "v0.1.0"},
		} {
			t.Run(client.Name+"/"+tc.name, func(t *testing.T) {
				core, logs := observer.New(zap.DebugLevel)
				mw := middleware.ClientVersion(tc.serverVersion, clientversion.Known, zap.New(core))
				before := outdatedCounts(t)

				w := serveClientVersion(t, client.UserAgent(tc.clientVersion), mw)

				assert.Equal(t, http.StatusOK, w.Code)
				if tc.wantLabel == "" {
					assert.Empty(t, w.Header().Get(clientversion.WarningHeader))
					assert.Zero(t, logs.Len())
					assert.Equal(t, before, outdatedCounts(t))
					return
				}

				assert.Equal(t, client.Warning(tc.clientVersion, tc.serverVersion), w.Header().Get(clientversion.WarningHeader))

				require.Equal(t, 1, logs.Len())
				entry := logs.All()[0]
				assert.Equal(t, zapcore.WarnLevel, entry.Level)
				assert.Equal(t, "outdated client", entry.Message)
				fields := entry.ContextMap()
				assert.Equal(t, client.Name, fields["client"])
				assert.Equal(t, tc.clientVersion, fields["clientVersion"])
				assert.Equal(t, tc.serverVersion, fields["serverVersion"])
				assert.Equal(t, "/test", fields["path"])
				assert.Equal(t, int64(http.StatusOK), fields["status"])
				assert.NotContains(t, fields, "user", "no Auth ran, so no user is known")

				want := maps.Clone(before)
				want[client.Name+"/"+tc.wantLabel]++
				assert.Equal(t, want, outdatedCounts(t))
			})
		}
	}
}

// TestClientVersionIgnoresUnknownClients checks that only clients in
// clientversion.Known are warned. The backend version is far ahead, so each
// of these requests would be warned if its client were checked.
func TestClientVersionIgnoresUnknownClients(t *testing.T) {
	others := []string{
		"Go-http-client/1.1", // capp-mcp and cappctl builds without the version check
		"curl/8.9.1",
		"Mozilla/5.0 (X11; Linux x86_64)",
		"kube-probe/1.30",
		"",
	}
	userAgents := make([]string, 0, len(others)+3*len(clientversion.Known))
	userAgents = append(userAgents, others...)
	for _, client := range clientversion.Known {
		userAgents = append(userAgents,
			client.Name+"-fork/v0.1.0",              // similar name
			"proxy/1.0 "+client.UserAgent("v0.1.0"), // known client, but not the first product token
			client.Name+"/",                         // known client without a version
		)
	}

	core, logs := observer.New(zap.DebugLevel)
	mw := middleware.ClientVersion("v99.0.0", clientversion.Known, zap.New(core))
	before := outdatedCounts(t)

	for _, userAgent := range userAgents {
		t.Run(userAgent, func(t *testing.T) {
			_, _, known := clientversion.FromUserAgent(userAgent, clientversion.Known)
			require.False(t, known, "%q is a known client; this test needs an unknown one", userAgent)

			w := serveClientVersion(t, userAgent, mw)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Empty(t, w.Header().Get(clientversion.WarningHeader))
		})
	}
	assert.Zero(t, logs.Len())
	assert.Equal(t, before, outdatedCounts(t))
}

// TestClientVersionChecksAnyListedClient shows that listing a client is all
// it takes to check it: the same request is warned when its client is in the
// list and ignored when it is not.
func TestClientVersionChecksAnyListedClient(t *testing.T) {
	frontend := clientversion.Client{Name: "capp-frontend-test"}
	userAgent := frontend.UserAgent("v0.4.0")

	w := serveClientVersion(t, userAgent, middleware.ClientVersion("v0.6.2", []clientversion.Client{frontend}, zap.NewNop()))
	assert.Equal(t, frontend.Warning("v0.4.0", "v0.6.2"), w.Header().Get(clientversion.WarningHeader))

	w = serveClientVersion(t, userAgent, middleware.ClientVersion("v0.6.2", clientversion.Known, zap.NewNop()))
	assert.Empty(t, w.Header().Get(clientversion.WarningHeader))
}

// TestClientVersionMetricVersionLabelIsBounded checks that made-up versions
// do not each create a metric series. Callers choose their User-Agent and the
// check runs before authentication.
func TestClientVersionMetricVersionLabelIsBounded(t *testing.T) {
	probe := clientversion.Client{Name: "label-bound-probe"}
	mw := middleware.ClientVersion("v1.2.0", []clientversion.Client{probe}, zap.NewNop())
	before := outdatedCounts(t)

	for i := range 200 {
		serveClientVersion(t, probe.UserAgent(fmt.Sprintf("v0.%d.%d", i, i)), mw)
		serveClientVersion(t, probe.UserAgent(fmt.Sprintf("v1.%d.%d", i%2, i)), mw)
	}

	got := map[string]float64{}
	for key, n := range outdatedCounts(t) {
		if name, version, _ := strings.Cut(key, "/"); name == probe.Name {
			got[version] = n - before[key]
		}
	}
	assert.Equal(t, map[string]float64{"v0.x": 200, "v1.0": 100, "v1.1": 100}, got)
}

// TestClientVersionLogsAuthenticatedUser checks that the warning log names
// the user Auth resolved, for every client in clientversion.Known, and never
// the bearer token.
func TestClientVersionLogsAuthenticatedUser(t *testing.T) {
	const token = "secret-bearer-token"
	authenticate := func(cred auth.ClusterCredential) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(string(middleware.CredentialKey), cred)
			c.Next()
		}
	}
	reject := func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }

	for _, client := range clientversion.Known {
		for _, tc := range []struct {
			name string
			// auth stands in for the Auth middleware; nil means a route
			// without authentication.
			auth       gin.HandlerFunc
			wantUser   string // empty means no user field
			wantStatus int
		}{
			{
				name:       "openshift or ldap user",
				auth:       authenticate(auth.ClusterCredential{ImpersonateUser: "alice", ImpersonateGroups: []string{"devs"}}),
				wantUser:   "alice",
				wantStatus: http.StatusOK,
			},
			{name: "passthrough token", auth: authenticate(auth.ClusterCredential{BearerToken: token}), wantStatus: http.StatusOK},
			{name: "authentication failed", auth: reject, wantStatus: http.StatusUnauthorized},
			{name: "route without authentication", wantStatus: http.StatusOK},
		} {
			t.Run(client.Name+"/"+tc.name, func(t *testing.T) {
				core, logs := observer.New(zap.DebugLevel)
				handlers := []gin.HandlerFunc{middleware.ClientVersion("v0.6.2", clientversion.Known, zap.New(core))}
				if tc.auth != nil {
					handlers = append(handlers, tc.auth)
				}

				w := serveClientVersion(t, client.UserAgent("v0.5.2"), handlers...)

				assert.Equal(t, tc.wantStatus, w.Code)
				assert.Equal(t, client.Warning("v0.5.2", "v0.6.2"), w.Header().Get(clientversion.WarningHeader))

				require.Equal(t, 1, logs.Len())
				fields := logs.All()[0].ContextMap()
				assert.Equal(t, int64(tc.wantStatus), fields["status"])
				if tc.wantUser == "" {
					assert.NotContains(t, fields, "user")
				} else {
					assert.Equal(t, tc.wantUser, fields["user"])
				}
				for key, value := range fields {
					assert.NotContains(t, fmt.Sprint(value), token, "field %q leaks the bearer token", key)
				}
			})
		}
	}
}

func TestClientVersionWarnsOnAbortedRequests(t *testing.T) {
	for _, client := range clientversion.Known {
		t.Run(client.Name, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, engine := gin.CreateTestContext(w)
			engine.Use(middleware.ClientVersion("v0.6.2", clientversion.Known, zap.NewNop()))
			engine.Use(func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
			engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("User-Agent", client.UserAgent("v0.5.2"))
			engine.ServeHTTP(w, req)

			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Equal(t, client.Warning("v0.5.2", "v0.6.2"), w.Header().Get(clientversion.WarningHeader))
		})
	}
}

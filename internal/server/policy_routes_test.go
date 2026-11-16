package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps/revisions"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/cfunc/pools"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/secrets"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewPolicyRouteCoverage(t *testing.T) {
	for _, mode := range []string{config.AuthModeLDAP, config.AuthModeOpenShift} {
		for _, optional := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s optional=%t", mode, optional), func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Auth.Mode = mode
				cfg.Resources.Namespaces.Enabled = true
				cfg.Metrics.Enabled, cfg.Metrics.Path = optional, "/monitoring/custom"
				cfg.Resources.Cfunc.Enabled = optional
				registry := policyRegistry(t, optional)
				srv, err := New(cfg, "dev", &testutil.MockAuthManager{}, &testutil.MockClusterManager{}, registry, zap.NewNop())
				if err != nil {
					t.Fatal(err)
				}
				engine, ok := srv.httpServer.Handler.(*gin.Engine)
				require.True(t, ok)
				metricsPath := ""
				if optional {
					metricsPath = cfg.Metrics.Path
				}
				rules, err := policy.NewRoutes(metricsPath)
				require.NoError(t, err)
				for _, route := range engine.Routes() {
					rule, exists := rules.Lookup(route.Method, route.Path)
					require.True(t, exists, "%s %s must have exactly one policy rule", route.Method, route.Path)
					switch rule.Category {
					case policy.Namespace, policy.Discovery, policy.Aggregate, policy.Migration:
						want := []string{authorization.RoleEditor}
						if route.Method == http.MethodGet {
							want = []string{authorization.RoleViewer, authorization.RoleEditor}
						}
						require.Equal(t, want, rule.AllowedRoles, "%s %s", route.Method, route.Path)
					default:
						require.Empty(t, rule.AllowedRoles)
					}
				}
			})
		}
	}
}

func policyRegistry(t testing.TB, optional bool) *resources.Registry {
	r := resources.NewRegistry(map[string]bool{"configmaps": optional, "secrets": optional, "cfuncpools": optional})
	r.Register(newCappsHandler(t, false, nil, nil, config.ObservabilityConfig{}))
	r.Register(revisions.New(false, nil, config.CappSizes{}))
	r.Register(configmaps.New())
	r.Register(secrets.New())
	poolHandler, err := pools.New(config.CfuncConfig{})
	require.NoError(t, err)
	r.Register(poolHandler)
	return r
}

// newCappsHandler builds a capps handler for policy tests, failing the test
// when the observability config cannot produce a Prometheus client.
func newCappsHandler(t testing.TB, gitopsEnabled bool, git capps.GitOpsSyncer, mgr cluster.ClusterManager, observability config.ObservabilityConfig) *capps.Handler {
	t.Helper()
	h, err := capps.New(gitopsEnabled, git, mgr, config.CappSizes{}, observability)
	require.NoError(t, err)
	return h
}

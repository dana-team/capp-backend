package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources"
	nshandler "github.com/dana-team/capp-backend/internal/resources/cluster/namespaces"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/benchmarks"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps/revisions"
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
				cfg.Metrics.Enabled, cfg.Metrics.Path = optional, "/monitoring/custom"
				registry := policyRegistry(optional)
				srv := New(cfg, "dev", &testutil.MockAuthManager{}, &testutil.MockClusterManager{}, registry, zap.NewNop())
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

func policyRegistry(optional bool) *resources.Registry {
	r := resources.NewRegistry(map[string]bool{"benchmarks": optional, "configmaps": optional, "secrets": optional})
	r.Register(nshandler.New(zap.NewNop()))
	r.Register(capps.New(false, nil, nil, config.CappSizes{}))
	r.Register(revisions.New(false, nil, config.CappSizes{}))
	r.Register(configmaps.New())
	r.Register(secrets.New())
	r.Register(benchmarks.New(config.BenchmarkConfig{}, zap.NewNop()))
	return r
}

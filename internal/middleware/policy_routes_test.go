package middleware_test

import (
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNewPolicyRoutes(t *testing.T) {
	for _, path := range []string{
		"/api/v1/clusters", "relative", "/metrics/:name",
		"/monitor//metrics", "/monitor/../healthz", "/metrics//", "//",
	} {
		_, err := middleware.NewPolicyRoutes(path)
		require.Error(t, err, "invalid or conflicting metrics path %q", path)
	}
	for _, path := range []string{"/", "/monitor/metrics", "/monitor/metrics/"} {
		rules, err := middleware.NewPolicyRoutes(path)
		require.NoError(t, err)
		router := gin.New()
		router.GET(path, func(*gin.Context) {})
		route := router.Routes()[0]
		rule, exists := rules.Lookup(route.Method, route.Path)
		require.True(t, exists, "registered metrics path %q must have a rule", route.Path)
		require.Equal(t, middleware.PolicyPublic, rule.Category)
	}
}

func TestPolicyRoutesLookup(t *testing.T) {
	const policyNamespaceRoute = "/api/v1/clusters/:cluster/namespaces/:namespace"
	rules, err := middleware.NewPolicyRoutes("/monitor")
	require.NoError(t, err)
	for _, tt := range []struct {
		method, route string
		category      middleware.PolicyCategory
		exists        bool
	}{
		{http.MethodGet, "/healthz", middleware.PolicyPublic, true},
		{http.MethodGet, "/monitor", middleware.PolicyPublic, true},
		{http.MethodGet, "/monitor/private", middleware.PolicyDenied, false},
		{http.MethodPost, "/monitor", middleware.PolicyDenied, false},
		{http.MethodGet, "/api/v1/clusters", middleware.PolicyDiscovery, true},
		{http.MethodGet, "/api/v1/clusters/:cluster", middleware.PolicyDiscovery, true},
		{http.MethodGet, "/api/v1/clusters/:cluster/namespaces", middleware.PolicyDiscovery, true},
		{http.MethodGet, "/api/v1/clusters/:cluster/secrets", middleware.PolicyAggregate, true},
		{http.MethodGet, policyNamespaceRoute, middleware.PolicyNamespace, true},
		{http.MethodGet, policyNamespaceRoute + "/secrets", middleware.PolicyNamespace, true},
		{http.MethodPost, policyNamespaceRoute + "/secrets", middleware.PolicyNamespace, true},
		{http.MethodPost, policyNamespaceRoute + "/capps/:name/migrate", middleware.PolicyMigration, true},
		{http.MethodPost, "/api/v1/clusters/:cluster/namespaces", middleware.PolicyDenied, true},
		{http.MethodPut, policyNamespaceRoute, middleware.PolicyDenied, true},
		{http.MethodPatch, policyNamespaceRoute, middleware.PolicyDenied, true},
		{http.MethodDelete, policyNamespaceRoute, middleware.PolicyDenied, true},
		{http.MethodGet, policyNamespaceRoute + "/capps/:name/benchmark", middleware.PolicyDenied, true},
		{http.MethodPost, policyNamespaceRoute + "/capps/:name/benchmark", middleware.PolicyDenied, true},
		{http.MethodGet, policyNamespaceRoute + "/capps/:name/metrics/query", middleware.PolicyDenied, true},
		{http.MethodGet, "/api/v1/clusters/prod/namespaces/team-a/secrets", middleware.PolicyDenied, false},
		{http.MethodGet, "/unknown", middleware.PolicyDenied, false},
	} {
		t.Run(tt.method+" "+tt.route, func(t *testing.T) {
			rule, exists := rules.Lookup(tt.method, tt.route)
			require.Equal(t, tt.exists, exists)
			require.Equal(t, tt.category, rule.Category)
			if tt.category == middleware.PolicyDenied || tt.category == middleware.PolicyPublic {
				require.Empty(t, rule.AllowedRoles)
			}
		})
	}
	rule, ok := rules.Lookup(http.MethodGet, policyNamespaceRoute+"/secrets")
	require.True(t, ok)
	rule.AllowedRoles[0] = "changed"
	again, _ := rules.Lookup(http.MethodGet, policyNamespaceRoute+"/secrets")
	require.Equal(t, authorization.RoleViewer, again.AllowedRoles[0])
}

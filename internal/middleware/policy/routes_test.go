package policy_test

import (
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNewRoutes(t *testing.T) {
	rules, err := policy.NewRoutes("")
	require.NoError(t, err)
	_, exists := rules.Lookup(http.MethodGet, "/")
	require.False(t, exists, "disabled metrics must not add a root route")
	for _, path := range []string{
		"/healthz", "/api", "/api/", "/api/v1/clusters/prod", "relative", "/metrics/:name",
		"/monitor//metrics", "/monitor/../healthz", "/metrics//", "//",
	} {
		_, err := policy.NewRoutes(path)
		require.Error(t, err, "invalid or conflicting metrics path %q", path)
	}
	for _, path := range []string{"/", "/monitor/metrics", "/monitor/metrics/"} {
		rules, err := policy.NewRoutes(path)
		require.NoError(t, err)
		router := gin.New()
		router.GET(path, func(*gin.Context) {})
		route := router.Routes()[0]
		rule, exists := rules.Lookup(route.Method, route.Path)
		require.True(t, exists, "registered metrics path %q must have a rule", route.Path)
		require.Equal(t, policy.Public, rule.Category)
	}
}

func TestRoutesLookup(t *testing.T) {
	const policyNamespaceRoute = "/api/v1/clusters/:cluster/namespaces/:namespace"
	rules, err := policy.NewRoutes("/monitor")
	require.NoError(t, err)
	for _, tt := range []struct {
		method, route string
		category      policy.Category
		exists        bool
	}{
		{http.MethodGet, "/healthz", policy.Public, true},
		{http.MethodGet, "/monitor", policy.Public, true},
		{http.MethodGet, "/monitor/private", policy.Denied, false},
		{http.MethodPost, "/monitor", policy.Denied, false},
		{http.MethodGet, "/api/v1/clusters", policy.Discovery, true},
		{http.MethodGet, "/api/v1/clusters/:cluster", policy.Discovery, true},
		{http.MethodGet, "/api/v1/clusters/:cluster/namespaces", policy.Discovery, true},
		{http.MethodGet, "/api/v1/clusters/:cluster/secrets", policy.Aggregate, true},
		{http.MethodGet, policyNamespaceRoute, policy.Namespace, true},
		{http.MethodGet, policyNamespaceRoute + "/secrets", policy.Namespace, true},
		{http.MethodPost, policyNamespaceRoute + "/secrets", policy.Namespace, true},
		{http.MethodPost, policyNamespaceRoute + "/capps/:name/migrate", policy.Migration, true},
		{http.MethodPost, "/api/v1/clusters/:cluster/namespaces", policy.Administration, true},
		{http.MethodPut, policyNamespaceRoute, policy.Administration, true},
		{http.MethodPatch, policyNamespaceRoute, policy.Administration, true},
		{http.MethodDelete, policyNamespaceRoute, policy.Administration, true},
		{http.MethodGet, policyNamespaceRoute + "/capps/:name/benchmark", policy.Denied, true},
		{http.MethodPost, policyNamespaceRoute + "/capps/:name/benchmark", policy.Denied, true},
		{http.MethodGet, policyNamespaceRoute + "/capps/:name/metrics/query", policy.Denied, true},
		{http.MethodGet, "/api/v1/clusters/prod/namespaces/team-a/secrets", policy.Denied, false},
		{http.MethodGet, "/unknown", policy.Denied, false},
	} {
		t.Run(tt.method+" "+tt.route, func(t *testing.T) {
			rule, exists := rules.Lookup(tt.method, tt.route)
			require.Equal(t, tt.exists, exists)
			require.Equal(t, tt.category, rule.Category)
			if tt.category == policy.Denied || tt.category == policy.Public || tt.category == policy.Administration {
				require.Empty(t, rule.AllowedRoles)
			}
		})
	}
	rule, ok := rules.Lookup(http.MethodGet, policyNamespaceRoute+"/secrets")
	require.True(t, ok)
	rule.AllowedRoles[0] = "changed"
	again, _ := rules.Lookup(http.MethodGet, policyNamespaceRoute+"/secrets")
	require.Equal(t, authorization.RoleViewer, again.AllowedRoles[0])
	for _, route := range []struct {
		method, path string
		roles        []string
	}{
		{http.MethodPost, policyNamespaceRoute + "/capps/:name/rollout", []string{authorization.RoleEditor}},
		{http.MethodDelete, policyNamespaceRoute + "/capps/:name/rollout/strategy", []string{authorization.RoleEditor}},
		{http.MethodGet, policyNamespaceRoute + "/configmaps/:name/bound", []string{authorization.RoleViewer, authorization.RoleEditor}},
		{http.MethodGet, policyNamespaceRoute + "/secrets/:name/bound", []string{authorization.RoleViewer, authorization.RoleEditor}},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rule, exists := rules.Lookup(route.method, route.path)
			require.True(t, exists)
			require.Equal(t, policy.Namespace, rule.Category)
			require.Equal(t, route.roles, rule.AllowedRoles)
		})
	}
}

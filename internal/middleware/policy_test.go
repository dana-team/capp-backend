package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequireExecution(t *testing.T) {
	for _, tt := range []struct {
		name    string
		change  func(*gin.Context, *policyDecision)
		allowed bool
	}{
		{"same request operation and target", func(*gin.Context, *policyDecision) {}, true},
		{"missing decision", func(c *gin.Context, _ *policyDecision) { delete(c.Keys, string(policyDecisionKey)) }, false},
		{"wrong context type", func(c *gin.Context, _ *policyDecision) { c.Set(string(policyDecisionKey), "allowed") }, false},
		{"other request", func(c *gin.Context, d *policyDecision) { d.request = c.Request.Clone(c.Request.Context()) }, false},
		{"changed operation", func(_ *gin.Context, d *policyDecision) { d.method = http.MethodDelete }, false},
		{"changed route", func(_ *gin.Context, d *policyDecision) { d.route += "/sync" }, false},
		{"changed cluster", func(_ *gin.Context, d *policyDecision) { d.cluster = "other" }, false},
		{"changed namespace", func(_ *gin.Context, d *policyDecision) { d.namespace = "other" }, false},
		{"administration decision cannot authorize resource execution", func(_ *gin.Context, d *policyDecision) { d.category = PolicyAdministration; d.operator = true }, false},
		{"aggregate cannot use ordinary execution", func(_ *gin.Context, d *policyDecision) { d.category = PolicyAggregate }, false},
		{"legacy request", func(c *gin.Context, _ *policyDecision) {
			delete(c.Keys, string(backendPolicyKey))
			delete(c.Keys, string(policyDecisionKey))
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/:cluster/:namespace", func(c *gin.Context) {
				c.Set(string(backendPolicyKey), true)
				d := policyDecision{request: c.Request, method: c.Request.Method, route: c.FullPath(), cluster: c.Param("cluster"), namespace: c.Param("namespace"), category: PolicyNamespace}
				c.Set(string(policyDecisionKey), d)
				tt.change(c, &d)
				// Preserve deliberate removal/type corruption of the context entry.
				if _, ok := c.Keys[string(policyDecisionKey)].(policyDecision); ok {
					c.Set(string(policyDecisionKey), d)
				}
				require.Equal(t, tt.allowed, RequireExecution(c))
				if tt.allowed {
					c.Status(http.StatusNoContent)
				}
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/c1/team-a", nil))
			if tt.allowed {
				require.Equal(t, http.StatusNoContent, w.Code)
			} else {
				require.Equal(t, http.StatusForbidden, w.Code)
				require.Contains(t, w.Body.String(), "POLICY_DENIED")
			}
		})
	}
}

func TestRequireNamespaceAccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*gin.Context, *policyDecision)
		allowed bool
	}{
		{"matching decision", func(*gin.Context, *policyDecision) {}, true},
		{"missing decision", func(c *gin.Context, _ *policyDecision) { delete(c.Keys, string(policyDecisionKey)) }, false},
		{"different request", func(c *gin.Context, d *policyDecision) { d.request = c.Request.Clone(c.Request.Context()) }, false},
		{"different operation", func(_ *gin.Context, d *policyDecision) { d.method = http.MethodDelete }, false},
		{"different cluster", func(_ *gin.Context, d *policyDecision) { d.cluster = "other" }, false},
		{"different namespace", func(_ *gin.Context, d *policyDecision) { d.namespace = "other" }, false},
		{"ordinary resource decision", func(_ *gin.Context, d *policyDecision) { d.category = PolicyNamespace }, false},
		{"nonoperator administration", func(_ *gin.Context, d *policyDecision) { d.operator = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.PUT("/:cluster/:namespace", func(c *gin.Context) {
				c.Set(string(backendPolicyKey), true)
				d := policyDecision{request: c.Request, method: c.Request.Method, route: c.FullPath(), cluster: c.Param("cluster"), namespace: c.Param("namespace"), category: PolicyAdministration, operator: true}
				c.Set(string(policyDecisionKey), d)
				tc.change(c, &d)
				if _, ok := c.Get(string(policyDecisionKey)); ok {
					c.Set(string(policyDecisionKey), d)
				}
				_, _, allowed := RequireNamespaceAccess(c)
				require.Equal(t, tc.allowed, allowed)
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/c1/team-a", nil))
			if tc.allowed {
				require.Equal(t, http.StatusOK, w.Code)
			} else {
				require.Equal(t, http.StatusForbidden, w.Code)
			}
		})
	}
}

func TestRequireDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, target          string
		category              PolicyCategory
		missing, otherRequest bool
		global                bool
		err                   error
		allowed               bool
		status                int
	}{
		{name: "matching discovery", target: "c1", category: PolicyDiscovery, allowed: true, status: http.StatusOK},
		{name: "different target", target: "c2", category: PolicyDiscovery, status: http.StatusForbidden},
		{name: "missing decision", target: "c1", category: PolicyDiscovery, missing: true, status: http.StatusForbidden},
		{name: "different request", target: "c1", category: PolicyDiscovery, otherRequest: true, status: http.StatusForbidden},
		{name: "resource decision", target: "c1", category: PolicyNamespace, status: http.StatusForbidden},
		{name: "global discovery skips unavailable target", target: "c1", category: PolicyDiscovery, global: true, err: authorization.ErrUnavailable, allowed: true, status: http.StatusOK},
		{name: "scoped discovery reports unavailable target", target: "c1", category: PolicyDiscovery, err: authorization.ErrUnavailable, status: http.StatusServiceUnavailable},
		{name: "global discovery preserves denial", target: "c1", category: PolicyDiscovery, global: true, err: authorization.ErrDenied, status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			called := false
			route, path := "/api/v1/clusters/:cluster", "/api/v1/clusters/c1"
			if tc.global {
				route, path = "/api/v1/clusters", "/api/v1/clusters"
			}
			router.GET(route, func(c *gin.Context) {
				c.Set(string(backendPolicyKey), true)
				d := policyDecision{request: c.Request, method: c.Request.Method, route: c.FullPath(), cluster: c.Param("cluster"), category: tc.category,
					discover: func(string) ([]authorization.NamespaceState, bool, error) { called = true; return nil, true, tc.err },
				}
				if tc.otherRequest {
					d.request = c.Request.Clone(c.Request.Context())
				}
				if !tc.missing {
					c.Set(string(policyDecisionKey), d)
				}
				states, operator, ok := RequireDiscovery(c, tc.target)
				require.Equal(t, tc.allowed, ok)
				require.Equal(t, tc.allowed && tc.err == nil, operator)
				if tc.allowed {
					require.Empty(t, states)
					require.False(t, c.Writer.Written())
					require.False(t, c.IsAborted())
					require.False(t, c.GetBool(string(policyDeniedKey)))
				}
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, tc.allowed || (tc.err != nil && !tc.missing), called)
			require.Equal(t, tc.status, w.Code)
		})
	}
}

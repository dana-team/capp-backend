package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

package policy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestListAggregate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*gin.Context, *policyDecision)
		allowed bool
	}{
		{"matching request", func(*gin.Context, *policyDecision) {}, true},
		{"missing mode", func(c *gin.Context, _ *policyDecision) { delete(c.Keys, string(backendPolicyKey)) }, false},
		{"missing decision", func(c *gin.Context, _ *policyDecision) { delete(c.Keys, string(policyDecisionKey)) }, false},
		{"different request", func(c *gin.Context, d *policyDecision) { d.request = c.Request.Clone(c.Request.Context()) }, false},
		{"different method", func(_ *gin.Context, d *policyDecision) { d.method = http.MethodPost }, false},
		{"different route", func(_ *gin.Context, d *policyDecision) { d.route += "/other" }, false},
		{"different cluster", func(_ *gin.Context, d *policyDecision) { d.cluster = "other" }, false},
		{"namespace decision", func(_ *gin.Context, d *policyDecision) { d.category = Namespace }, false},
		{"missing collection authorization", func(_ *gin.Context, d *policyDecision) { d.aggregate = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			called := false
			r.GET("/:cluster/secrets", func(c *gin.Context) {
				c.Set(string(backendPolicyKey), true)
				d := policyDecision{request: c.Request, method: c.Request.Method, route: c.FullPath(), cluster: c.Param("cluster"), category: Aggregate}
				d.aggregate = func(*gin.Context, client.ObjectList, ...client.ListOption) error { called = true; return nil }
				c.Set(string(policyDecisionKey), d)
				tc.change(c, &d)
				if _, ok := c.Get(string(policyDecisionKey)); ok {
					c.Set(string(policyDecisionKey), d)
				}
				require.Equal(t, tc.allowed, ListAggregate(c, &corev1.SecretList{}))
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/c1/secrets", nil))
			require.Equal(t, tc.allowed, called)
			if !tc.allowed {
				require.Equal(t, http.StatusForbidden, w.Code)
				require.Contains(t, w.Body.String(), apierrors.ReasonPolicyDenied)
			}
		})
	}
}

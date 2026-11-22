package namespaces

import (
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNewAdministration(t *testing.T) {
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{})
	prefixes := []string{"system:"}
	admin, err := NewAdministration(storage, "capp-system", nil, prefixes)
	require.NoError(t, err)
	prefixes[0] = "changed:"
	users := []string{"system:admin"}
	_, err = admin.prepareMembership(t.Context(), "team-a", UpdateNamespaceRequest{Users: &users}, false)
	require.ErrorContains(t, err, "invalid namespace membership")
	_, err = NewAdministration(storage, "capp-system", nil, []string{""})
	require.ErrorContains(t, err, "empty prefix")
}

// Without a policy decision, namespace routes must fail closed.
func TestHandlerRegisterRoutes(t *testing.T) {
	handler := NewWithAdministration(zap.NewNop(), nil)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/api/v1/clusters/:cluster"))
	for _, route := range []struct{ method, namespace string }{
		{http.MethodGet, ""},
		{http.MethodGet, "team-a"},
		{http.MethodPost, ""},
		{http.MethodPut, "team-a"},
		{http.MethodPatch, "team-a"},
		{http.MethodDelete, "team-a"},
	} {
		t.Run(route.method+"/"+route.namespace, func(t *testing.T) {
			response := membershipRequest(router, route.method, route.namespace, `{"name":"team-a","users":["alice"]}`)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s namespace %q without policy decision = %d, want %d: %s", route.method, route.namespace, response.Code, http.StatusForbidden, response.Body.String())
			}
		})
	}
}

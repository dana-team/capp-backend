package namespaces

import (
	"testing"

	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/require"
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

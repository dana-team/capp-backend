package namespaced

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestExtractClient(t *testing.T) {
	for _, value := range []any{nil, "not-a-client", testutil.FakeClient(t)} {
		w, c := testutil.GinTestContext(t)
		if value != nil {
			c.Set(string(middleware.K8sClientKey), value)
		}
		require.Nil(t, ExtractClient(c))
		require.Equal(t, http.StatusForbidden, w.Code)
		var response struct {
			Error apierrors.APIError `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Equal(t, apierrors.ReasonPolicyDenied, response.Error.Details["reason"])
	}
}

// Package trackingtest builds trackers for handler tests.
package trackingtest

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/desiredstate/desiredstatetest"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// New returns a tracker on an empty in-memory store.
func New(t testing.TB) (*tracking.Tracker, *desiredstatetest.MemoryStore) {
	t.Helper()
	sealer, err := desiredstate.NewSealer("k1", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)})
	require.NoError(t, err)
	store := desiredstatetest.NewMemoryStore()
	return tracking.New(store, sealer, time.Second), store
}

// Engine serves the handlers below /api/v1/clusters/:cluster for user alice.
// Both the user and the admin client are k8sClient.
func Engine(t *testing.T, k8sClient client.Client, handlers ...testutil.RouteRegistrar) *testutil.EngineHelper {
	t.Helper()
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), k8sClient)
		c.Set(string(middleware.AdminK8sClientKey), k8sClient)
		c.Set(string(middleware.ClusterMetaKey), cluster.ClusterMeta{Name: c.Param("cluster")})
		c.Set(string(middleware.CredentialKey), auth.ClusterCredential{ImpersonateUser: "alice"})
	})
	group := engine.Group("/api/v1/clusters/:cluster")
	for _, handler := range handlers {
		handler.RegisterRoutes(group)
	}
	return testutil.NewEngineHelperFrom(t, engine)
}

// Drift decodes the drift of a resource response or of an error's details.
func Drift(t testing.TB, w *httptest.ResponseRecorder) *drift.Drift {
	t.Helper()
	var body struct {
		Drift *drift.Drift `json:"drift"`
		Error *struct {
			Details struct {
				Drift *drift.Drift `json:"drift"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	if body.Error != nil {
		return body.Error.Details.Drift
	}
	return body.Drift
}

// Action builds the body of a revert or accept for an observed drift.
func Action(d *drift.Drift) drift.ActionRequest {
	return drift.ActionRequest{Generation: d.Generation, LiveVersion: d.LiveVersion}
}

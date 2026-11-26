package secrets

import (
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/resources/tracking/trackingtest"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSecretDrift(t *testing.T) {
	const listPath = "/api/v1/clusters/c1/namespaces/team-a/secrets"
	const itemPath = listPath + "/db"
	k8sClient := testutil.FakeClient(t)
	tracker, store := trackingtest.New(t)
	e := trackingtest.Engine(t, k8sClient, New().WithTracker(tracker))

	w := e.PostJSON(listPath, SecretRequest{Name: "db", Data: map[string]string{"password": "first", "user": "app"}})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	record, err := store.Get(t.Context(), desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindSecret, Namespace: "team-a", Name: "db"})
	require.NoError(t, err)
	assert.NotContains(t, record.Desired.JSON, "first")
	require.Len(t, record.Desired.SecretData, 2)
	assert.Equal(t, "alice", record.UpdatedBy)

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: "team-a", Name: "db"}
	require.NoError(t, k8sClient.Get(t.Context(), key, secret))
	secret.Data["password"] = []byte("second")
	require.NoError(t, k8sClient.Update(t.Context(), secret))

	d := trackingtest.Drift(t, e.Get(itemPath))
	assert.Equal(t, []drift.Field{{Path: "/data/password"}}, d.Fields)

	w = e.PutJSON(itemPath, SecretUpdateRequest{Data: map[string]string{"password": "third"}})
	require.Equal(t, http.StatusConflict, w.Code)
	assert.NotContains(t, w.Body.String(), "first")
	assert.NotContains(t, w.Body.String(), "second")

	w = e.PostJSON(itemPath+"/drift/revert", trackingtest.Action(d))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "first")
	require.NoError(t, k8sClient.Get(t.Context(), key, secret))
	assert.Equal(t, "first", string(secret.Data["password"]))
}

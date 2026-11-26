package revisions

import (
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/resources/tracking/trackingtest"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRollbackRecordsTheResult(t *testing.T) {
	capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"}}
	capp.Spec.State = "disabled"
	revision := &cappv1alpha1.CappRevision{ObjectMeta: metav1.ObjectMeta{
		Name: "app-00001", Namespace: "team-a", Labels: map[string]string{k8s.LabelCappName: "app"},
	}}
	revision.Spec.RevisionNumber = 1
	revision.Spec.CappTemplate.Spec.State = "enabled"

	tracker, store := trackingtest.New(t)
	e := trackingtest.Engine(t, testutil.FakeClient(t, capp, revision), New(config.CappSizes{}).WithTracker(tracker))
	w := e.Post("/api/v1/clusters/c1/namespaces/team-a/capps/app/revisions/1/rollback", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	record, err := store.Get(t.Context(), desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindCapp, Namespace: "team-a", Name: "app"})
	require.NoError(t, err)
	assert.Contains(t, record.Desired.JSON, `"state":"enabled"`)

	store.Err = assert.AnError
	w = e.Post("/api/v1/clusters/c1/namespaces/team-a/capps/app/revisions/1/rollback", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed)
}

package capps

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const rolloutPath = "/namespaces/ns1/capps/app1/rollout"

// rollingOutCapp returns a Capp with a strategy whose candidate app-00002 is in the given phase.
func rollingOutCapp(phase cappv1alpha1.RolloutPhase) *cappv1alpha1.Capp {
	capp := makeCapp("app1", "ns1")
	capp.Spec.RouteSpec.RolloutStrategy = &cappv1alpha1.RolloutStrategy{
		Stages: []cappv1alpha1.RolloutStage{{Percent: 50}},
	}
	capp.Status.Rollout = &cappv1alpha1.RolloutStatus{
		StableRevision:    "app-00001",
		CandidateRevision: "app-00002",
		Phase:             phase,
	}
	return capp
}

func TestValidateRolloutAction(t *testing.T) {
	const (
		progressing = cappv1alpha1.RolloutPhaseProgressing
		holding     = cappv1alpha1.RolloutPhaseHolding
		promoting   = cappv1alpha1.RolloutPhasePromoting
		failed      = cappv1alpha1.RolloutPhaseFailed
		rolledBack  = cappv1alpha1.RolloutPhaseRolledBack
		aborted     = cappv1alpha1.RolloutPhaseAborted
		stable      = cappv1alpha1.RolloutPhaseStable
	)
	held := func(c *cappv1alpha1.Capp) {
		percent := int64(10)
		c.Status.Rollout.HeldPercent = &percent
	}

	tests := []struct {
		name    string
		action  string
		phases  []cappv1alpha1.RolloutPhase
		mutate  func(*cappv1alpha1.Capp)
		wantErr bool
	}{
		{name: "promote active", action: "promote", phases: []cappv1alpha1.RolloutPhase{progressing, holding, promoting, failed}},
		{name: "promote finished", action: "promote", phases: []cappv1alpha1.RolloutPhase{rolledBack, aborted, stable}, wantErr: true},
		{name: "abort active", action: "abort", phases: []cappv1alpha1.RolloutPhase{progressing, holding, promoting, failed}},
		{name: "abort finished", action: "abort", phases: []cappv1alpha1.RolloutPhase{rolledBack, aborted, stable}, wantErr: true},
		{name: "retry stopped", action: "retry", phases: []cappv1alpha1.RolloutPhase{failed, rolledBack, aborted}},
		{name: "retry held", action: "retry", phases: []cappv1alpha1.RolloutPhase{holding}, mutate: held},
		{name: "retry not held", action: "retry", phases: []cappv1alpha1.RolloutPhase{holding}, wantErr: true},
		{name: "retry running", action: "retry", phases: []cappv1alpha1.RolloutPhase{progressing, promoting, stable}, wantErr: true},
		{
			name:    "no strategy",
			action:  "promote",
			phases:  []cappv1alpha1.RolloutPhase{progressing},
			mutate:  func(c *cappv1alpha1.Capp) { c.Spec.RouteSpec.RolloutStrategy = nil },
			wantErr: true,
		},
		{
			name:    "no rollout status",
			action:  "promote",
			phases:  []cappv1alpha1.RolloutPhase{progressing},
			mutate:  func(c *cappv1alpha1.Capp) { c.Status.Rollout = nil },
			wantErr: true,
		},
		{
			name:    "no candidate",
			action:  "promote",
			phases:  []cappv1alpha1.RolloutPhase{progressing},
			mutate:  func(c *cappv1alpha1.Capp) { c.Status.Rollout.CandidateRevision = "" },
			wantErr: true,
		},
		{
			name:   "action pending",
			action: "abort",
			phases: []cappv1alpha1.RolloutPhase{progressing},
			mutate: func(c *cappv1alpha1.Capp) {
				c.Annotations = map[string]string{k8s.AnnotationRollout: "promote:app-00002"}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		for _, phase := range tt.phases {
			t.Run(tt.name+"/"+string(phase), func(t *testing.T) {
				capp := rollingOutCapp(phase)
				if tt.mutate != nil {
					tt.mutate(capp)
				}

				err := validateRolloutAction(capp, tt.action, "app-00002")

				if !tt.wantErr {
					assert.NoError(t, err)
					return
				}
				var apiErr *apierrors.APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, http.StatusConflict, apiErr.Status)
			})
		}
	}
}

func TestValidateRolloutAction_StaleRevisionNamesCandidate(t *testing.T) {
	err := validateRolloutAction(rollingOutCapp(cappv1alpha1.RolloutPhaseProgressing), "promote", "app-00001")

	var apiErr *apierrors.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Contains(t, apiErr.Message, "app-00002")
}

func TestRolloutAction_SetsAnnotation(t *testing.T) {
	k8sClient := testutil.FakeClient(t, rollingOutCapp(cappv1alpha1.RolloutPhaseProgressing))
	e := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, false, nil, nil, makeSizes()))

	w := e.PostJSON(rolloutPath, RolloutActionRequest{Action: "promote", Revision: "app-00002"})

	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	updated, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.Equal(t, "promote:app-00002", updated.Annotations[k8s.AnnotationRollout])
}

func TestRolloutAction_BadBody(t *testing.T) {
	for _, body := range []string{
		`{bad`,
		`{"action":"pause","revision":"app-00002"}`,
		`{"action":"promote"}`,
		`{"revision":"app-00002"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := engine(t, rollingOutCapp(cappv1alpha1.RolloutPhaseProgressing)).Post(rolloutPath, bytes.NewBufferString(body))
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestRolloutAction_NotFound(t *testing.T) {
	w := engine(t).PostJSON(rolloutPath, RolloutActionRequest{Action: "promote", Revision: "app-00002"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRolloutAction_Conflict(t *testing.T) {
	k8sClient := fake.NewClientBuilder().
		WithScheme(testutil.TestScheme(t)).
		WithObjects(rollingOutCapp(cappv1alpha1.RolloutPhaseProgressing)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				gr := schema.GroupResource{Group: "rcs.dana.io", Resource: "capps"}
				return k8serrors.NewConflict(gr, "app1", errors.New("changed"))
			},
		}).Build()
	e := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, false, nil, nil, makeSizes()))

	w := e.PostJSON(rolloutPath, RolloutActionRequest{Action: "promote", Revision: "app-00002"})

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestRolloutAction_GitSynced_NoGitWrite(t *testing.T) {
	capp := rollingOutCapp(cappv1alpha1.RolloutPhaseProgressing)
	capp.Labels = map[string]string{k8s.LabelBackupToGit: "true"}
	mock := &mockGitOpsSyncer{}

	w := syncEngine(t, mock, cluster.ClusterMeta{Name: "test"}, makeSizes(), capp).
		PostJSON(rolloutPath, RolloutActionRequest{Action: "abort", Revision: "app-00002"})

	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Empty(t, mock.syncCalls)
}

func TestRemoveRolloutStrategy_Success(t *testing.T) {
	k8sClient := testutil.FakeClient(t, rollingOutCapp(cappv1alpha1.RolloutPhaseProgressing))
	e := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, false, nil, nil, makeSizes()))

	w := e.Delete(rolloutPath + "/strategy")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updated, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.Nil(t, updated.Spec.RouteSpec.RolloutStrategy)
}

func TestRemoveRolloutStrategy_NoStrategy(t *testing.T) {
	// Writes fail on this client, so a 200 proves the handler did not write.
	e := testutil.NewEngineHelper(t, failWrites(t, makeCapp("app1", "ns1")), mustHandler(t, false, nil, nil, makeSizes()))

	w := e.Delete(rolloutPath + "/strategy")

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestRemoveRolloutStrategy_NotFound(t *testing.T) {
	w := engine(t).Delete(rolloutPath + "/strategy")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRemoveRolloutStrategy_GitSynced(t *testing.T) {
	gitSynced := func() *cappv1alpha1.Capp {
		capp := rollingOutCapp(cappv1alpha1.RolloutPhaseProgressing)
		capp.Labels = map[string]string{k8s.LabelBackupToGit: "true"}
		return capp
	}

	t.Run("syncs values without the strategy", func(t *testing.T) {
		mock := &mockGitOpsSyncer{}
		w := syncEngine(t, mock, cluster.ClusterMeta{Name: "test"}, makeSizes(), gitSynced()).
			Delete(rolloutPath + "/strategy")

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, mock.syncCalls, 1)
		assert.NotContains(t, string(mock.syncCalls[0]), "rolloutStrategy")
	})

	t.Run("sync failure is a warning", func(t *testing.T) {
		mock := &mockGitOpsSyncer{
			syncFn: func(context.Context, string, string, string, []byte) (string, error) {
				return "", errors.New("push failed")
			},
		}
		w := syncEngine(t, mock, cluster.ClusterMeta{Name: "test"}, makeSizes(), gitSynced()).
			Delete(rolloutPath + "/strategy")

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), apierrors.CodeGitopsFailed)
	})
}

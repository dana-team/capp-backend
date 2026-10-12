package revisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cappSpec(image string) cappv1alpha1.CappSpec {
	var spec cappv1alpha1.CappSpec
	spec.ConfigurationSpec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: image}}
	return spec
}

// makeCapp returns the live Capp ns1/app1 (image nginx:2) that the revisions in these tests belong to.
func makeCapp(labels map[string]string) *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "app1", Namespace: "ns1", Labels: labels},
		Spec:       cappSpec("nginx:2"),
	}
}

func makeRevision(cappName, namespace string, number int, image string, labels map[string]string) *cappv1alpha1.CappRevision {
	return &cappv1alpha1.CappRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%05d", cappName, number),
			Namespace: namespace,
			Labels:    map[string]string{k8s.LabelCappName: cappName},
		},
		Spec: cappv1alpha1.CappRevisionSpec{
			RevisionNumber: number,
			CappTemplate: cappv1alpha1.CappTemplate{
				Spec:        cappSpec(image),
				Labels:      labels,
				Annotations: map[string]string{"note": fmt.Sprintf("rev-%d", number)},
			},
		},
	}
}

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New(false, nil, config.CappSizes{}))
}

type mockGitOpsSyncer struct {
	syncErr   error
	syncCalls int
}

func (m *mockGitOpsSyncer) SyncValues(_ context.Context, _, _, _ string, _ []byte) (string, error) {
	m.syncCalls++
	return "abc123", m.syncErr
}

func (m *mockGitOpsSyncer) DeleteValues(_ context.Context, _, _, _ string) (string, error) {
	return "def456", nil
}

func (m *mockGitOpsSyncer) BuildRelPath(gitOpsPath, namespace, cappName string) string {
	return gitOpsPath + "/" + namespace + "/" + cappName
}

func syncEngine(t *testing.T, mock *mockGitOpsSyncer, objects ...client.Object) *testutil.EngineHelper {
	t.Helper()
	k8sClient := testutil.FakeClient(t, objects...)
	return testutil.NewEngineHelperWithAdmin(t, k8sClient, k8sClient, cluster.ClusterMeta{Name: "test"}, New(true, mock, config.CappSizes{}))
}

func TestList_SortedNewestFirstAndFilteredByCapp(t *testing.T) {
	w := engine(t,
		makeRevision("app1", "ns1", 1, "nginx:1", nil),
		makeRevision("app1", "ns1", 3, "nginx:3", nil),
		makeRevision("app1", "ns1", 2, "nginx:2", nil),
		makeRevision("other", "ns1", 1, "busybox", nil),
		makeRevision("app1", "ns2", 9, "nginx:9", nil),
	).Get("/namespaces/ns1/capps/app1/revisions")

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappRevisionListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 3, resp.Total)
	assert.Equal(t, []int{3, 2, 1}, []int{resp.Items[0].RevisionNumber, resp.Items[1].RevisionNumber, resp.Items[2].RevisionNumber})
	assert.Equal(t, "nginx:3", resp.Items[0].Image)
	assert.Equal(t, "app1-00003", resp.Items[0].Name)
	assert.Equal(t, "rev-3", resp.Items[0].Annotations["note"])
}

func TestList_Empty(t *testing.T) {
	w := engine(t).Get("/namespaces/ns1/capps/app1/revisions")

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappRevisionListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Total)
	assert.NotNil(t, resp.Items)
}

func TestGet(t *testing.T) {
	w := engine(t,
		makeRevision("app1", "ns1", 1, "nginx:1", nil),
		makeRevision("app1", "ns1", 2, "nginx:2", nil),
	).Get("/namespaces/ns1/capps/app1/revisions/1")

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappRevisionResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.RevisionNumber)
	assert.Equal(t, "nginx:1", resp.Image)
}

func TestGet_NotFound(t *testing.T) {
	w := engine(t, makeRevision("app1", "ns1", 1, "nginx:1", nil)).
		Get("/namespaces/ns1/capps/app1/revisions/7")

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGet_InvalidRevision(t *testing.T) {
	for _, rev := range []string{"abc", "0", "-1"} {
		t.Run(rev, func(t *testing.T) {
			w := engine(t).Get("/namespaces/ns1/capps/app1/revisions/" + rev)
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestRollback_AppliesTemplate(t *testing.T) {
	k8sClient := testutil.FakeClient(t,
		makeCapp(map[string]string{"team": "b"}),
		makeRevision("app1", "ns1", 1, "nginx:1", map[string]string{"team": "a"}),
	)
	w := testutil.NewEngineHelper(t, k8sClient, New(false, nil, config.CappSizes{})).
		Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "app1", resp.Name)
	assert.Equal(t, "nginx:1", resp.Image)

	var live cappv1alpha1.Capp
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "app1"}, &live))
	assert.Equal(t, "nginx:1", live.Spec.ConfigurationSpec.Template.Spec.Containers[0].Image)
	assert.Equal(t, map[string]string{"team": "a"}, live.Labels)
	assert.Equal(t, "rev-1", live.Annotations["note"])
}

func TestRollback_RevisionNotFound(t *testing.T) {
	w := engine(t, makeCapp(nil)).
		Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRollback_CappNotFound(t *testing.T) {
	w := engine(t, makeRevision("app1", "ns1", 1, "nginx:1", nil)).
		Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "Capp")
}

func TestApplyTemplate_KeepsLiveBackupLabel(t *testing.T) {
	tests := []struct {
		name       string
		liveLabels map[string]string
		tmplLabels map[string]string
		want       map[string]string
	}{
		{
			name:       "live backed up, revision not",
			liveLabels: map[string]string{k8s.LabelBackupToGit: "true"},
			tmplLabels: map[string]string{"team": "a"},
			want:       map[string]string{"team": "a", k8s.LabelBackupToGit: "true"},
		},
		{
			name:       "revision backed up, live not",
			liveLabels: nil,
			tmplLabels: map[string]string{"team": "a", k8s.LabelBackupToGit: "true"},
			want:       map[string]string{"team": "a"},
		},
		{
			name:       "nil template labels",
			liveLabels: map[string]string{k8s.LabelBackupToGit: "true"},
			tmplLabels: nil,
			want:       map[string]string{k8s.LabelBackupToGit: "true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live := makeCapp(tt.liveLabels)
			got := applyTemplate(live, cappv1alpha1.CappTemplate{Spec: cappSpec("nginx:1"), Labels: tt.tmplLabels})
			assert.Equal(t, tt.want, got.Labels)
			assert.Equal(t, tt.liveLabels, live.Labels, "live object must not be mutated")
		})
	}
}

func TestRollback_GitSynced_PushesValues(t *testing.T) {
	mock := &mockGitOpsSyncer{}
	w := syncEngine(t, mock,
		makeCapp(map[string]string{k8s.LabelBackupToGit: "true"}),
		makeRevision("app1", "ns1", 1, "nginx:1", nil),
	).Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, mock.syncCalls)
	var resp capps.CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Warnings)
}

func TestRollback_GitSyncFails_ReturnsWarning(t *testing.T) {
	mock := &mockGitOpsSyncer{syncErr: errors.New("push rejected")}
	w := syncEngine(t, mock,
		makeCapp(map[string]string{k8s.LabelBackupToGit: "true"}),
		makeRevision("app1", "ns1", 1, "nginx:1", nil),
	).Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Warnings, 1)
	assert.Equal(t, apierrors.CodeGitopsFailed, resp.Warnings[0].Code)
}

func TestRollback_NotGitSynced_SkipsGit(t *testing.T) {
	mock := &mockGitOpsSyncer{}
	w := syncEngine(t, mock,
		makeCapp(nil),
		makeRevision("app1", "ns1", 1, "nginx:1", nil),
	).Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, mock.syncCalls)
}

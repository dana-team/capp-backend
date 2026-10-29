package capps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/secrets"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func makeCapp(name, namespace string) *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func makeCappWithLabel(name, namespace string, labels map[string]string) *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
	}
}

func makeSizes() config.CappSizes {
	small := config.ResourceSize{
		Requests: config.ResourceQuantities{CPU: "100m", Memory: "128Mi"},
		Limits:   config.ResourceQuantities{CPU: "200m", Memory: "256Mi"},
	}
	medium := config.ResourceSize{
		Requests: config.ResourceQuantities{CPU: "200m", Memory: "256Mi"},
		Limits:   config.ResourceQuantities{CPU: "400m", Memory: "512Mi"},
	}
	large := config.ResourceSize{
		Requests: config.ResourceQuantities{CPU: "400m", Memory: "512Mi"},
		Limits:   config.ResourceQuantities{CPU: "800m", Memory: "1Gi"},
	}
	return config.CappSizes{
		Small:  small,
		Medium: medium,
		Large:  large,
	}
}

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New(false, nil, nil, makeSizes()))
}

// syncEngine creates an engine with gitops enabled, ClusterMeta in context,
// and a mock GitOpsSyncer.
func syncEngine(t *testing.T, mock *mockGitOpsSyncer, meta cluster.ClusterMeta, sizes config.CappSizes, objects ...client.Object) *testutil.EngineHelper {
	t.Helper()
	k8sClient := testutil.FakeClient(t, objects...)
	return syncEngineWithClient(t, mock, meta, sizes, k8sClient)
}

func syncEngineWithClient(t *testing.T, mock *mockGitOpsSyncer, meta cluster.ClusterMeta, sizes config.CappSizes, k8sClient client.Client) *testutil.EngineHelper {
	t.Helper()
	handler := New(true, mock, nil, sizes)
	return testutil.NewEngineHelperWithAdmin(t, k8sClient, k8sClient, meta, handler)
}

type mockGitOpsSyncer struct {
	syncFn       func(ctx context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte) (string, error)
	deleteFn     func(ctx context.Context, gitOpsPath, namespace, cappName string) (string, error)
	buildRelPath func(gitOpsPath, namespace, cappName string) string

	syncCalls   [][]byte
	deleteCalls int
}

func (m *mockGitOpsSyncer) SyncValues(ctx context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte) (string, error) {
	m.syncCalls = append(m.syncCalls, valuesYAML)
	if m.syncFn != nil {
		return m.syncFn(ctx, gitOpsPath, namespace, cappName, valuesYAML)
	}
	return "abc123", nil
}

func (m *mockGitOpsSyncer) DeleteValues(ctx context.Context, gitOpsPath, namespace, cappName string) (string, error) {
	m.deleteCalls++
	if m.deleteFn != nil {
		return m.deleteFn(ctx, gitOpsPath, namespace, cappName)
	}
	return "def456", nil
}

// failWrites returns a fake client whose Update, Patch and Delete calls fail
// while reads succeed.
func failWrites(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	writeErr := errors.New("write failed")
	return fake.NewClientBuilder().
		WithScheme(testutil.TestScheme(t)).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return writeErr
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return writeErr
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return writeErr
			},
		}).Build()
}

func getCapp(t *testing.T, k8sClient client.Client) (*cappv1alpha1.Capp, error) {
	t.Helper()
	var capp cappv1alpha1.Capp
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "app1"}, &capp)
	return &capp, err
}

func gitSyncedCapp() *cappv1alpha1.Capp {
	return makeCappWithLabel("app1", "ns1", map[string]string{k8s.LabelBackupToGit: "true"})
}

func (m *mockGitOpsSyncer) BuildRelPath(gitOpsPath, namespace, cappName string) string {
	if m.buildRelPath != nil {
		return m.buildRelPath(gitOpsPath, namespace, cappName)
	}
	return filepath.Join("sites", gitOpsPath, namespace, cappName+".yaml")
}

// -- ListAll tests --

func TestListAll_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Get("/capps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

func TestListAll_Empty(t *testing.T) {
	w := engine(t).Get("/capps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Total)
}

// -- List by namespace tests --

func TestList_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1"), makeCapp("app2", "ns2")).
		Get("/namespaces/ns1/capps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
	assert.Equal(t, "app1", resp.Items[0].Name)
}

// -- Get tests --

func TestGet_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Get("/namespaces/ns1/capps/app1")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "app1", resp.Name)
}

func TestGet_NotFound(t *testing.T) {
	w := engine(t).Get("/namespaces/ns1/capps/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Create tests --

func TestCreate_Success(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/ns1/capps",
		CappRequest{Name: "new-app", Image: "nginx"})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-app", resp.Name)
	assert.Equal(t, "ns1", resp.Namespace)
}

func TestCreate_BadJSON(t *testing.T) {
	w := engine(t).Post("/namespaces/ns1/capps", bytes.NewBufferString("{invalid"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Update tests --

func TestUpdate_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).PutJSON("/namespaces/ns1/capps/app1",
		CappRequest{Name: "app1", Image: "nginx:2"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	w := engine(t).PutJSON("/namespaces/ns1/capps/missing",
		CappRequest{Name: "missing", Image: "nginx"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Put("/namespaces/ns1/capps/app1",
		bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdate_PreservesMetadata(t *testing.T) {
	capp := makeCapp("app1", "ns1")
	capp.Labels = map[string]string{k8s.LabelBackupToGit: "true", "team": "alpha"}
	capp.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "app:rcs.dana.io/Capp:ns1/app1"}
	capp.Finalizers = []string{"dana.io/capp-cleanup"}

	k8sClient := testutil.FakeClient(t, capp)
	e := testutil.NewEngineHelper(t, k8sClient, New(false, nil, nil, makeSizes()))

	w := e.PutJSON("/namespaces/ns1/capps/app1", CappRequest{Name: "app1", Image: "nginx:2"})
	require.Equal(t, http.StatusOK, w.Code)

	updated, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.Equal(t, capp.Labels, updated.Labels)
	assert.Equal(t, capp.Annotations, updated.Annotations)
	assert.Equal(t, capp.Finalizers, updated.Finalizers)
}

func TestUpdate_GitSyncEnabled_SyncsNewValues(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}
	k8sClient := testutil.FakeClient(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		PutJSON("/namespaces/ns1/capps/app1", CappRequest{Name: "app1", Image: "nginx:2"})

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, mock.syncCalls, 1)
	assert.Contains(t, string(mock.syncCalls[0]), "nginx:2")

	updated, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.Equal(t, "nginx:2", updated.Spec.ConfigurationSpec.Template.Spec.Containers[0].Image)
	assert.True(t, k8s.HasBackupLabel(updated.Labels))
}

func TestUpdate_BackupFailure_SucceedsWithWarning(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{
		syncFn: func(context.Context, string, string, string, []byte) (string, error) {
			return "", errors.New("push failed")
		},
	}
	k8sClient := testutil.FakeClient(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		PutJSON("/namespaces/ns1/capps/app1", CappRequest{Name: "app1", Image: "nginx:2"})

	require.Equal(t, http.StatusOK, w.Code, "the cluster write succeeded; only the backup is stale")
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Warnings, 1)
	assert.Equal(t, apierrors.CodeGitopsFailed, resp.Warnings[0].Code)

	live, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.Equal(t, "nginx:2", live.Spec.ConfigurationSpec.Template.Spec.Containers[0].Image)
}

func TestUpdate_BackupSucceeds_NoWarnings(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}

	w := syncEngine(t, &mockGitOpsSyncer{}, meta, makeSizes(), gitSyncedCapp()).
		PutJSON("/namespaces/ns1/capps/app1", CappRequest{Name: "app1", Image: "nginx:2"})

	require.Equal(t, http.StatusOK, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Warnings)
}

func TestUpdate_GitSyncNotEnabled_SkipsGit(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}

	w := syncEngine(t, mock, meta, makeSizes(), makeCapp("app1", "ns1")).
		PutJSON("/namespaces/ns1/capps/app1", CappRequest{Name: "app1", Image: "nginx:2"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, mock.syncCalls)
	assert.Zero(t, mock.deleteCalls)
}

func TestUpdate_ClusterWriteFailure_SkipsGit(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}
	k8sClient := failWrites(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		PutJSON("/namespaces/ns1/capps/app1", CappRequest{Name: "app1", Image: "nginx:2"})

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, mock.syncCalls, "git must not be written when the cluster write fails")
}

// -- Delete tests --

func TestDelete_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Delete("/namespaces/ns1/capps/app1")
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDelete_NotFound(t *testing.T) {
	w := engine(t).Delete("/namespaces/ns1/capps/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDelete_GitSyncEnabled_DeletesValues(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}
	k8sClient := testutil.FakeClient(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Delete("/namespaces/ns1/capps/app1")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, 1, mock.deleteCalls)
	_, err := getCapp(t, k8sClient)
	assert.True(t, k8serrors.IsNotFound(err))
}

func TestDelete_BackupFailure_SucceedsWithWarning(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{
		deleteFn: func(context.Context, string, string, string) (string, error) {
			return "", errors.New("push failed")
		},
	}
	k8sClient := testutil.FakeClient(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Delete("/namespaces/ns1/capps/app1")

	require.Equal(t, http.StatusOK, w.Code, "the Capp is gone, which is what was asked for")
	var resp DeleteResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Warnings, 1)
	assert.Equal(t, apierrors.CodeGitopsFailed, resp.Warnings[0].Code)

	_, err := getCapp(t, k8sClient)
	assert.True(t, k8serrors.IsNotFound(err))
}

func TestDelete_ClusterWriteFailure_SkipsGit(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}
	k8sClient := failWrites(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Delete("/namespaces/ns1/capps/app1")

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Zero(t, mock.deleteCalls)
}

func TestDelete_GitSyncNotEnabled_SkipsGit(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}

	w := syncEngine(t, mock, meta, makeSizes(), makeCapp("app1", "ns1")).
		Delete("/namespaces/ns1/capps/app1")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Zero(t, mock.deleteCalls)
}

// -- respondList tests --

func TestRespondList_CorrectTotalAndMapping(t *testing.T) {
	w := engine(t, makeCapp("a", "ns1"), makeCapp("b", "ns1")).
		Get("/namespaces/ns1/capps")

	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Total)
	assert.Len(t, resp.Items, 2)
}

// -- Sync tests --

func TestSync_GitOpsDisabled(t *testing.T) {
	h := New(false, nil, nil, makeSizes())
	e := testutil.NewEngineHelper(t, testutil.FakeClient(t, makeCapp("app1", "ns1")), h)
	w := e.Post("/namespaces/ns1/capps/app1/sync", nil)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestSync_CappNotFound(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test1"}
	w := syncEngine(t, &mockGitOpsSyncer{}, meta, makeSizes()).
		Post("/namespaces/ns1/capps/missing/sync", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSync_ReSync(t *testing.T) {
	capp := makeCappWithLabel("app1", "ns1", map[string]string{
		k8s.LabelBackupToGit: "true",
	})
	capp.Spec.ConfigurationSpec.Template.Spec.Containers = []corev1.Container{
		{Name: "main", Image: "nginx:9"},
	}
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test1"}
	mock := &mockGitOpsSyncer{}

	w := syncEngine(t, mock, meta, makeSizes(), capp).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SyncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "abc123", resp.CommitSHA)
	assert.NotEmpty(t, resp.Path)

	// Syncing an already-synced Capp is a manual re-push of whatever the
	// cluster holds right now, not a no-op.
	require.Len(t, mock.syncCalls, 1)
	assert.Contains(t, string(mock.syncCalls[0]), "nginx:9")
}

func TestSync_Success(t *testing.T) {
	capp := makeCapp("app1", "ns1")
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test"}
	mock := &mockGitOpsSyncer{}

	e := syncEngine(t, mock, meta, makeSizes(), capp)
	w := e.Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SyncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "abc123", resp.CommitSHA)
	assert.Equal(t, "sites/test/ns1/app1.yaml", resp.Path)
}

func TestSync_GitPushError(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test1"}
	mock := &mockGitOpsSyncer{
		syncFn: func(_ context.Context, _, _, _ string, _ []byte) (string, error) {
			return "", errors.New("push failed")
		},
	}
	k8sClient := testutil.FakeClient(t, makeCapp("app1", "ns1"))

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	require.Equal(t, http.StatusBadGateway, w.Code)
	live, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.False(t, k8s.HasBackupLabel(live.Labels),
		"git is written first: when it fails the Capp must not be marked as backed up")
}

func TestSync_PatchFailure_AfterGitWrite(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}
	k8sClient := failWrites(t, makeCapp("app1", "ns1"))

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Len(t, mock.syncCalls, 1, "git is written before the label patch")
}

// -- Unsync tests --

func TestUnsync_RemovesLabelAndValues(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}
	capp := makeCappWithLabel("app1", "ns1", map[string]string{k8s.LabelBackupToGit: "true", "team": "alpha"})
	k8sClient := testutil.FakeClient(t, capp)

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Delete("/namespaces/ns1/capps/app1/sync")

	require.Equal(t, http.StatusOK, w.Code)
	var resp SyncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Enabled)
	assert.Equal(t, "def456", resp.CommitSHA)
	assert.Equal(t, "sites/test/ns1/app1.yaml", resp.Path)
	assert.Equal(t, 1, mock.deleteCalls)

	live, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.False(t, k8s.HasBackupLabel(live.Labels))
	assert.Equal(t, "alpha", live.Labels["team"], "other labels must be kept")
}

func TestUnsync_NotEnabled_Idempotent(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{
		deleteFn: func(context.Context, string, string, string) (string, error) { return "", nil },
	}

	w := syncEngine(t, mock, meta, makeSizes(), makeCapp("app1", "ns1")).
		Delete("/namespaces/ns1/capps/app1/sync")

	require.Equal(t, http.StatusOK, w.Code)
	var resp SyncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Enabled)
	assert.Empty(t, resp.CommitSHA)

	// Git is still asked to remove the file, so an orphaned values file left by
	// an earlier failure can be cleaned up even though the label is gone.
	assert.Equal(t, 1, mock.deleteCalls)
}

func TestUnsync_GitOpsDisabled(t *testing.T) {
	w := engine(t, gitSyncedCapp()).Delete("/namespaces/ns1/capps/app1/sync")
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestUnsync_CappNotFound(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	w := syncEngine(t, &mockGitOpsSyncer{}, meta, makeSizes()).
		Delete("/namespaces/ns1/capps/missing/sync")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUnsync_GitFailure_ReturnsBadGateway(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{
		deleteFn: func(context.Context, string, string, string) (string, error) {
			return "", errors.New("push failed")
		},
	}
	k8sClient := testutil.FakeClient(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Delete("/namespaces/ns1/capps/app1/sync")

	assert.Equal(t, http.StatusBadGateway, w.Code)
	live, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.True(t, k8s.HasBackupLabel(live.Labels),
		"git is written first: when it fails the label must survive, or the values file is orphaned")
}

func TestSync_SuccessVerifyLabel(t *testing.T) {
	capp := makeCapp("app1", "ns1")
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test1"}
	mock := &mockGitOpsSyncer{}

	k8sClient := testutil.FakeClient(t, capp)
	handler := New(true, mock, nil, makeSizes())
	e := testutil.NewEngineHelperWithAdmin(t, k8sClient, k8sClient, meta, handler)

	w := e.Post("/namespaces/ns1/capps/app1/sync", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var updated cappv1alpha1.Capp
	err := k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "ns1", Name: "app1",
	}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "true", updated.Labels[k8s.LabelBackupToGit])
}

func TestSync_UsesClusterNameNotGitOpsPath(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		gitOpsPath  string
		namespace   string
		cappName    string
		wantPath    string
	}{
		{
			name:        "cluster name differs from gitOpsPath",
			clusterName: "production-east",
			gitOpsPath:  "east",
			namespace:   "team-alpha",
			cappName:    "web-api",
			wantPath:    "sites/production-east/team-alpha/web-api.yaml",
		},
		{
			name:        "cluster name equals gitOpsPath",
			clusterName: "staging",
			gitOpsPath:  "staging",
			namespace:   "apps",
			cappName:    "frontend",
			wantPath:    "sites/staging/apps/frontend.yaml",
		},
		{
			name:        "gitOpsPath is default but cluster name is not",
			clusterName: "managed-cluster-1",
			gitOpsPath:  "default",
			namespace:   "workloads",
			cappName:    "worker",
			wantPath:    "sites/managed-cluster-1/workloads/worker.yaml",
		},
		{
			name:        "namespace is not default",
			clusterName: "hub",
			gitOpsPath:  "hub-old",
			namespace:   "my-project",
			cappName:    "backend-svc",
			wantPath:    "sites/hub/my-project/backend-svc.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capp := makeCapp(tt.cappName, tt.namespace)
			meta := cluster.ClusterMeta{Name: tt.clusterName, GitOpsPath: tt.gitOpsPath}
			var capturedPath, capturedNS, capturedName string
			mock := &mockGitOpsSyncer{
				syncFn: func(_ context.Context, gitOpsPath, namespace, cappName string, _ []byte) (string, error) {
					capturedPath = gitOpsPath
					capturedNS = namespace
					capturedName = cappName
					return "sha-ok", nil
				},
			}

			w := syncEngine(t, mock, meta, makeSizes(), capp).
				Post("/namespaces/"+tt.namespace+"/capps/"+tt.cappName+"/sync", nil)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, tt.clusterName, capturedPath, "should use ClusterMeta.Name, not GitOpsPath")
			assert.Equal(t, tt.namespace, capturedNS, "should use capp.Namespace from K8s object")
			assert.Equal(t, tt.cappName, capturedName)

			var resp SyncResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, tt.wantPath, resp.Path)
		})
	}
}

// -- Migrate tests --

// migrateEngine creates an engine for migrate tests. It injects the source K8s
// client, credential, and cluster metadata into the Gin context.
func migrateEngine(t *testing.T, sourceClient client.Client, clusterMgr *testutil.MockClusterManager) *testutil.EngineHelper {
	t.Helper()
	meta := cluster.ClusterMeta{Name: "source-cluster"}
	handler := New(false, nil, clusterMgr, makeSizes())

	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), sourceClient)
		c.Set(string(middleware.AdminK8sClientKey), sourceClient)
		c.Set(string(middleware.ClusterMetaKey), meta)
		c.Set(string(middleware.CredentialKey), auth.ClusterCredential{BearerToken: "test-token"})
		c.Next()
	})
	handler.RegisterRoutes(engine.Group(""))
	return testutil.NewEngineHelperFrom(t, engine)
}

func TestMigrate(t *testing.T) {
	targetCC := &cluster.ClusterClient{}
	targetCC.SetHealthy(true)

	t.Run("success without delete", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		require.Equal(t, http.StatusOK, w.Code)
		var resp MigrateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "my-app", resp.Name)
		assert.Equal(t, "source-cluster", resp.SourceCluster)
		assert.Equal(t, "ns1", resp.SourceNamespace)
		assert.Equal(t, "target-cluster", resp.TargetCluster)
		assert.Equal(t, "target-ns", resp.TargetNamespace)
		assert.False(t, resp.SourceDeleted)

		// Source still exists
		var src cappv1alpha1.Capp
		require.NoError(t, sourceClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "my-app"}, &src))
	})

	t.Run("success with deleteSource", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns", DeleteSource: true,
		})

		require.Equal(t, http.StatusOK, w.Code)
		var resp MigrateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.True(t, resp.SourceDeleted)
	})

	t.Run("success with dependent resources", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "ns1",
			Labels: map[string]string{"dana.io/capp-managed": "true"},
		}}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "cm1", Namespace: "ns1",
			Labels: map[string]string{"dana.io/capp-managed": "true"},
		}}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp, secret, cm)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		require.Equal(t, http.StatusOK, w.Code)
		var resp MigrateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, []string{"s1"}, resp.CopiedSecrets)
		assert.Equal(t, []string{"cm1"}, resp.CopiedConfigMaps)
	})

	t.Run("target cluster not found", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		sourceClient := testutil.FakeClient(t, sourceCapp)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) {
				return nil, cluster.ErrClusterNotFound
			},
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "missing", TargetNamespace: "ns",
		})

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("target cluster unhealthy", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		sourceClient := testutil.FakeClient(t, sourceCapp)
		unhealthyCC := &cluster.ClusterClient{}
		unhealthyCC.SetHealthy(false)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return unhealthyCC, nil },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "sick", TargetNamespace: "ns",
		})

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})

	t.Run("target namespace denied", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		sourceClient := testutil.FakeClient(t, sourceCapp)

		mgr := &testutil.MockClusterManager{
			GetFn:                func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return false },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "forbidden-ns",
		})

		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("target namespace missing", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t) // no namespace

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "nonexistent",
		})

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("source capp not found", func(t *testing.T) {
		sourceClient := testutil.FakeClient(t) // no capp
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/missing/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("capp already exists on target", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		existingTarget := makeCapp("my-app", "target-ns")
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS, existingTarget)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("secret collision on target", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		sourceSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "ns1",
			Labels: map[string]string{"dana.io/capp-managed": "true"},
		}}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		targetSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp, sourceSecret)
		targetClient := testutil.FakeClient(t, targetNS, targetSecret)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("bad request body", func(t *testing.T) {
		sourceClient := testutil.FakeClient(t)
		mgr := &testutil.MockClusterManager{}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.Post("/namespaces/ns1/capps/my-app/migrate", bytes.NewBufferString(`{}`))

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("same cluster and namespace", func(t *testing.T) {
		sourceClient := testutil.FakeClient(t)
		mgr := &testutil.MockClusterManager{}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "source-cluster", TargetNamespace: "ns1",
		})

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("copy with hostname uses target hostname and skips bypass", func(t *testing.T) {
		sourceCapp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
			Spec:       cappv1alpha1.CappSpec{RouteSpec: cappv1alpha1.RouteSpec{Hostname: "app.example.com"}},
		}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
			TargetHostname: "new.example.com",
		})

		require.Equal(t, http.StatusOK, w.Code)
		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
		assert.Equal(t, "new.example.com", created.Spec.RouteSpec.Hostname)
		assert.NotContains(t, created.Annotations, migrationBypassAnnotation)
	})

	t.Run("bypass annotation not set without hostname", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		require.Equal(t, http.StatusOK, w.Code)
		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
		assert.NotContains(t, created.Annotations, migrationBypassAnnotation)
	})

	t.Run("bypass annotation removed after source delete", func(t *testing.T) {
		sourceCapp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
			Spec:       cappv1alpha1.CappSpec{RouteSpec: cappv1alpha1.RouteSpec{Hostname: "app.example.com"}},
		}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns", DeleteSource: true,
		})

		require.Equal(t, http.StatusOK, w.Code)
		var resp MigrateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.True(t, resp.SourceDeleted)

		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
		assert.NotContains(t, created.Annotations, migrationBypassAnnotation)
	})

	t.Run("no managed resources in namespace", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		require.Equal(t, http.StatusOK, w.Code)
		var resp MigrateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Empty(t, resp.CopiedSecrets)
		assert.Empty(t, resp.CopiedConfigMaps)
	})

	t.Run("cleans up copied resources when capp create fails", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "ns1",
			Labels: map[string]string{"dana.io/capp-managed": "true"},
		}}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "cm1", Namespace: "ns1",
			Labels: map[string]string{"dana.io/capp-managed": "true"},
		}}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp, secret, cm)
		targetClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*cappv1alpha1.Capp); ok {
					return errors.New("webhook denied capp")
				}
				return c.Create(ctx, obj, opts...)
			},
		}, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		assert.NotEqual(t, http.StatusOK, w.Code)

		getSecret := targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "s1"}, &corev1.Secret{})
		require.Error(t, getSecret, "secret should have been cleaned up")

		getCM := targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "cm1"}, &corev1.ConfigMap{})
		require.Error(t, getCM, "configmap should have been cleaned up")
	})

	t.Run("configmap collision on target", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		sourceCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "cm1", Namespace: "ns1",
			Labels: map[string]string{"dana.io/capp-managed": "true"},
		}}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		targetCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp, sourceCM)
		targetClient := testutil.FakeClient(t, targetNS, targetCM)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		assert.Equal(t, http.StatusConflict, w.Code)
	})

	failDeleteClient := func(t *testing.T, objs ...client.Object) client.Client {
		t.Helper()
		return fake.NewClientBuilder().
			WithScheme(testutil.TestScheme(t)).
			WithObjects(objs...).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					return errors.New("simulated delete failure")
				},
			}).Build()
	}

	t.Run("source delete fails after create", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := failDeleteClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns", DeleteSource: true,
		})

		require.Equal(t, http.StatusOK, w.Code)
		var resp MigrateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.SourceDeleted)

		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
	})

	t.Run("bypass annotation kept on source delete failure", func(t *testing.T) {
		sourceCapp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
			Spec:       cappv1alpha1.CappSpec{RouteSpec: cappv1alpha1.RouteSpec{Hostname: "app.example.com"}},
		}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := failDeleteClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns", DeleteSource: true,
		})

		require.Equal(t, http.StatusOK, w.Code)
		var resp MigrateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.SourceDeleted)

		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
		assert.Equal(t, "true", created.Annotations[migrationBypassAnnotation])
	})

	t.Run("copy with hostname without targetHostname rejected", func(t *testing.T) {
		sourceCapp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
			Spec:       cappv1alpha1.CappSpec{RouteSpec: cappv1alpha1.RouteSpec{Hostname: "app.example.com"}},
		}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
		})

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("copy with hostname same targetHostname rejected", func(t *testing.T) {
		sourceCapp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
			Spec:       cappv1alpha1.CappSpec{RouteSpec: cappv1alpha1.RouteSpec{Hostname: "app.example.com"}},
		}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
			TargetHostname: "app.example.com",
		})

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("copy without hostname rejects targetHostname", func(t *testing.T) {
		sourceCapp := makeCapp("my-app", "ns1")
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
			TargetHostname: "new.example.com",
		})

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("move with different targetHostname skips bypass", func(t *testing.T) {
		sourceCapp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
			Spec:       cappv1alpha1.CappSpec{RouteSpec: cappv1alpha1.RouteSpec{Hostname: "app.example.com"}},
		}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
			DeleteSource: true, TargetHostname: "new.example.com",
		})

		require.Equal(t, http.StatusOK, w.Code)
		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
		assert.Equal(t, "new.example.com", created.Spec.RouteSpec.Hostname)
		assert.NotContains(t, created.Annotations, migrationBypassAnnotation)
	})

	t.Run("move with same targetHostname uses bypass", func(t *testing.T) {
		sourceCapp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
			Spec:       cappv1alpha1.CappSpec{RouteSpec: cappv1alpha1.RouteSpec{Hostname: "app.example.com"}},
		}
		targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-ns"}}
		sourceClient := testutil.FakeClient(t, sourceCapp)
		targetClient := testutil.FakeClient(t, targetNS)

		mgr := &testutil.MockClusterManager{
			GetFn: func(name string) (*cluster.ClusterClient, error) { return targetCC, nil },
			ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
				return targetClient, nil
			},
			IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
		}
		e := migrateEngine(t, sourceClient, mgr)
		w := e.PostJSON("/namespaces/ns1/capps/my-app/migrate", MigrateRequest{
			TargetCluster: "target-cluster", TargetNamespace: "target-ns",
			DeleteSource: true, TargetHostname: "app.example.com",
		})

		require.Equal(t, http.StatusOK, w.Code)
		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
		assert.Equal(t, "app.example.com", created.Spec.RouteSpec.Hostname)
		assert.NotContains(t, created.Annotations, migrationBypassAnnotation)
	})
}

// TestHandlerListAll exercises the shared aggregate contract through each resource's routes.
func TestHandlerListAll(t *testing.T) {
	for _, resource := range []string{"capps", "secrets", "configmaps"} {
		t.Run(resource, func(t *testing.T) {
			for _, scenario := range []string{
				"filtered", "empty", "excluded only", "policy invalidated", "namespaces invalidated",
				"empty policy invalidated", "empty namespaces invalidated", "excluded policy invalidated",
				"excluded namespaces invalidated", "evaluation error", "recreated",
			} {
				t.Run(scenario, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						f := newAggregateFixture(t, resource, scenario)
						snapshot, err := f.store.Snapshot(auth.ClusterCredential{ImpersonateUser: "alice"})
						require.NoError(t, err)
						switch scenario {
						case "policy invalidated", "empty policy invalidated", "excluded policy invalidated":
							f.store.Invalidate()
						case "namespaces invalidated", "empty namespaces invalidated", "excluded namespaces invalidated":
							f.store.InvalidateNamespaces()
						}
						if scenario == "recreated" {
							var namespace corev1.Namespace
							require.NoError(t, f.base.Get(t.Context(), client.ObjectKey{Name: "allowed"}, &namespace))
							require.NoError(t, f.base.Delete(t.Context(), &namespace))
							namespace.UID, namespace.ResourceVersion = "replacement", ""
							require.NoError(t, f.base.Create(t.Context(), &namespace))
							// A quiet watch retains the predecessor identity without an expiry.
							time.Sleep(10 * time.Second)
							synctest.Wait()
							f.check(t, snapshot, http.StatusOK, []string{"allowed"})
							f.namespaceWatch.Modify(&namespace)
							synctest.Wait()
							current, err := f.store.Snapshot(auth.ClusterCredential{ImpersonateUser: "alice"})
							require.NoError(t, err)
							f.check(t, current, http.StatusOK, nil)
							f.check(t, snapshot, http.StatusServiceUnavailable, nil)
							return
						}
						status := http.StatusOK
						var names []string
						switch {
						case strings.Contains(scenario, "invalidated"), scenario == "evaluation error":
							status = http.StatusServiceUnavailable
						case scenario == "filtered":
							names = []string{"allowed"}
						}
						f.check(t, snapshot, status, names)
					})
				})
			}
		})
	}
}

type aggregateFixture struct {
	store          *authorization.Store
	base           client.Client
	target         client.WithWatch
	config         config.ClusterAuthorizationConfig
	namespaceWatch *watch.RaceFreeFakeWatcher
	resource       string
	request        context.Context
	resourceLists  int
}

func newAggregateFixture(t *testing.T, resource, scenario string) *aggregateFixture {
	t.Helper()
	f := &aggregateFixture{
		resource: resource, request: t.Context(),
		config: config.ClusterAuthorizationConfig{PolicyNamespace: "policies", ProtectedNamespaces: []string{"protected"}},
	}
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "capp-policy", Namespace: "policies",
		Labels: map[string]string{consts.CappAPIGroup + "/authorization": "true"},
	}, Data: map[string]string{"policy.json": `{"version":1,"roles":{"capp-user":[{"resource":"` + resource + `","actions":["list"]}]},"bindings":[]}`}}
	objects := []client.Object{policy}
	for _, name := range []string{"allowed", "denied", "protected", "excluded", "policies", "z-error"} {
		uid := types.UID("uid-" + name)
		objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}})
		if name == "denied" || (name == "z-error" && scenario != "evaluation error") {
			continue
		}
		memberName := name
		if name == "z-error" {
			memberName = "different-name"
		}
		payload, err := json.Marshal(map[string]any{
			"version": 1, "namespace": map[string]string{"name": memberName, "uid": string(uid)}, "users": []string{"alice"}, "bindings": []any{},
		})
		require.NoError(t, err)
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "capp-members-" + string(uid), Namespace: "policies", Labels: policy.Labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: memberName, UID: uid}},
		}, Data: map[string]string{"policy.json": string(payload)}})
	}
	names := []string{"allowed", "denied", "protected", "excluded", "policies", "missing"}
	switch {
	case strings.HasPrefix(scenario, "empty"):
		names = nil
	case strings.HasPrefix(scenario, "excluded"):
		names = []string{"excluded", "protected", "policies"}
	case scenario == "evaluation error":
		names = []string{"allowed", "z-error"}
	case scenario == "recreated":
		names = []string{"allowed"}
	}
	for _, name := range names {
		meta := metav1.ObjectMeta{Name: name, Namespace: name, Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}}
		switch resource {
		case "capps":
			objects = append(objects, &cappv1alpha1.Capp{ObjectMeta: meta})
		case "secrets":
			objects = append(objects, &corev1.Secret{ObjectMeta: meta, Data: map[string][]byte{"value": []byte(name)}})
		case "configmaps":
			objects = append(objects, &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"value": name}})
		}
	}
	if scenario == "filtered" {
		// Secrets and ConfigMaps retain their managed-resource selector.
		switch resource {
		case "secrets":
			objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: "allowed"}})
		case "configmaps":
			objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: "allowed"}})
		}
	}
	f.base = testutil.FakeClient(t, objects...)
	base, ok := f.base.(client.WithWatch)
	require.True(t, ok)
	f.target = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if ctx == f.request {
				var matches bool
				switch resource {
				case "capps":
					_, matches = list.(*cappv1alpha1.CappList)
				case "secrets":
					_, matches = list.(*corev1.SecretList)
				case "configmaps":
					_, matches = list.(*corev1.ConfigMapList)
				}
				if !matches {
					t.Fatalf("aggregate %s request performed unexpected LIST: %T", resource, list)
				}
				f.resourceLists++
				options := &client.ListOptions{}
				for _, opt := range opts {
					opt.ApplyToList(options)
				}
				if resource != "capps" && (options.LabelSelector == nil || options.LabelSelector.String() != consts.ManagedLabelKey+"="+consts.ManagedLabelValue) {
					t.Fatalf("aggregate %s selector = %v, want managed-resource selector", resource, options.LabelSelector)
				}
			}
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			list.SetResourceVersion("1")
			return nil
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("aggregate request performed unexpected GET")
			return nil
		},
		Watch: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
			stream := watch.NewRaceFreeFake()
			if _, ok := list.(*corev1.NamespaceList); ok {
				f.namespaceWatch = stream
			}
			return stream, nil
		},
	})
	var err error
	f.store, err = authorization.NewStore(f.target, f.config, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(f.store.Stop)
	require.NoError(t, f.store.Start(t.Context()))
	synctest.Wait()
	time.Sleep(time.Second)
	synctest.Wait()
	return f
}

func (f *aggregateFixture) check(t *testing.T, snapshot *authorization.Snapshot, status int, names []string) {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), f.target)
		c.Set(string(middleware.ClusterMetaKey), cluster.ClusterMeta{Name: "prod",
			AllowedNamespaces: []string{"allowed", "denied", "protected", "policies", "missing", "z-error"}})
		c.Set(string(middleware.AuthorizationKey), &middleware.AuthorizationState{Targets: map[string]*middleware.AuthorizationTarget{
			"prod": {Snapshot: snapshot, Client: f.target, Config: f.config},
		}})
	})
	group := router.Group("/api/v1/clusters/:cluster")
	switch f.resource {
	case "capps":
		New(false, nil, nil, config.CappSizes{}).RegisterRoutes(group)
	case "secrets":
		secrets.New().RegisterRoutes(group)
	case "configmaps":
		configmaps.New().RegisterRoutes(group)
	}
	response := httptest.NewRecorder()
	f.resourceLists = 0
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/prod/"+f.resource, nil).WithContext(f.request))
	assert.Equal(t, 1, f.resourceLists, "aggregate request resource LIST count")
	require.Equal(t, status, response.Code, response.Body.String())
	if status != http.StatusOK {
		assert.Contains(t, response.Body.String(), `"reason":"POLICY_UNAVAILABLE"`)
		assert.NotContains(t, response.Body.String(), `"items"`)
		return
	}
	var result struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(t, result.Items, len(names))
	assert.Equal(t, len(names), result.Total)
	for i, name := range names {
		assert.Equal(t, name, result.Items[i].Name)
	}
}

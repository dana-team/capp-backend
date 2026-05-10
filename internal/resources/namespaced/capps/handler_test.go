package capps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New(false, nil))
}

// syncEngine creates an engine with gitops enabled, ClusterMeta in context,
// and a mock GitOpsSyncer.
func syncEngine(t *testing.T, mock *mockGitOpsSyncer, meta cluster.ClusterMeta, objects ...client.Object) *testutil.EngineHelper {
	t.Helper()
	k8sClient := testutil.FakeClient(t, objects...)
	handler := New(true, mock)
	return testutil.NewEngineHelperWithAdmin(t, k8sClient, k8sClient, meta, handler)
}

type mockGitOpsSyncer struct {
	syncFn       func(ctx context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte) (string, error)
	buildRelPath func(gitOpsPath, namespace, cappName string) string
}

func (m *mockGitOpsSyncer) SyncValues(ctx context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte) (string, error) {
	if m.syncFn != nil {
		return m.syncFn(ctx, gitOpsPath, namespace, cappName, valuesYAML)
	}
	return "abc123", nil
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
		CappRequest{Name: "new-app", Namespace: "ns1", Image: "nginx"})

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

func TestCreate_NamespaceOverrideFromURL(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/correct-ns/capps",
		CappRequest{Name: "app", Namespace: "wrong-ns", Image: "nginx"})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "correct-ns", resp.Namespace)
}

// -- Update tests --

func TestUpdate_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).PutJSON("/namespaces/ns1/capps/app1",
		CappRequest{Name: "app1", Namespace: "ns1", Image: "nginx:2"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	w := engine(t).PutJSON("/namespaces/ns1/capps/missing",
		CappRequest{Name: "missing", Namespace: "ns1", Image: "nginx"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Put("/namespaces/ns1/capps/app1",
		bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
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
	h := New(false, nil)
	e := testutil.NewEngineHelper(t, testutil.FakeClient(t, makeCapp("app1", "ns1")), h)
	w := e.Post("/namespaces/ns1/capps/app1/sync", nil)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestSync_CappNotFound(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "nova"}
	w := syncEngine(t, &mockGitOpsSyncer{}, meta).
		Post("/namespaces/ns1/capps/missing/sync", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSync_ReSync(t *testing.T) {
	capp := makeCappWithLabel("app1", "ns1", map[string]string{
		k8s.LabelBackupToGit: "true",
	})
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "nova"}
	mock := &mockGitOpsSyncer{}

	w := syncEngine(t, mock, meta, capp).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp syncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "abc123", resp.CommitSHA)
	assert.NotEmpty(t, resp.Path)
}

func TestSync_Success(t *testing.T) {
	capp := makeCapp("app1", "ns1")
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "nova"}
	mock := &mockGitOpsSyncer{}

	e := syncEngine(t, mock, meta, capp)
	w := e.Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp syncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "abc123", resp.CommitSHA)
	assert.Equal(t, "sites/nova/ns1/app1.yaml", resp.Path)
}

func TestSync_GitPushError(t *testing.T) {
	capp := makeCapp("app1", "ns1")
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "nova"}
	mock := &mockGitOpsSyncer{
		syncFn: func(_ context.Context, _, _, _ string, _ []byte) (string, error) {
			return "", errors.New("push failed")
		},
	}

	w := syncEngine(t, mock, meta, capp).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestSync_SuccessVerifyLabel(t *testing.T) {
	capp := makeCapp("app1", "ns1")
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "nova"}
	mock := &mockGitOpsSyncer{}

	k8sClient := testutil.FakeClient(t, capp)
	handler := New(true, mock)
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

func TestSync_PassesCorrectGitOpsPath(t *testing.T) {
	capp := makeCapp("myapp", "production")
	meta := cluster.ClusterMeta{Name: "cluster-five", GitOpsPath: "five"}
	var capturedPath, capturedNS, capturedName string
	mock := &mockGitOpsSyncer{
		syncFn: func(_ context.Context, gitOpsPath, namespace, cappName string, _ []byte) (string, error) {
			capturedPath = gitOpsPath
			capturedNS = namespace
			capturedName = cappName
			return "def456", nil
		},
	}

	w := syncEngine(t, mock, meta, capp).
		Post("/namespaces/production/capps/myapp/sync", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "five", capturedPath)
	assert.Equal(t, "production", capturedNS)
	assert.Equal(t, "myapp", capturedName)

	var resp syncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "def456", resp.CommitSHA)
	assert.Equal(t, "sites/five/production/myapp.yaml", resp.Path)
}

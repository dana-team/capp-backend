package capps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New(false, nil, makeSizes()))
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
	handler := New(true, mock, sizes)
	return testutil.NewEngineHelperWithAdmin(t, k8sClient, k8sClient, meta, handler)
}

type mockGitOpsSyncer struct {
	syncFn       func(ctx context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte) (string, error)
	deleteFn     func(ctx context.Context, gitOpsPath, namespace, cappName string) (string, error)
	buildRelPath func(gitOpsPath, namespace, cappName string) string

	syncCalls   [][]byte
	deleteCalls int
}

func (m *mockGitOpsSyncer) SyncValues(ctx context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte, _ string) (string, error) {
	m.syncCalls = append(m.syncCalls, valuesYAML)
	if m.syncFn != nil {
		return m.syncFn(ctx, gitOpsPath, namespace, cappName, valuesYAML)
	}
	return "abc123", nil
}

func (m *mockGitOpsSyncer) DeleteValues(ctx context.Context, gitOpsPath, namespace, cappName, _ string) (string, error) {
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
	e := testutil.NewEngineHelper(t, k8sClient, New(false, nil, makeSizes()))

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

func TestUpdate_GitSyncFailure_ReturnsBadGateway(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{
		syncFn: func(context.Context, string, string, string, []byte) (string, error) {
			return "", errors.New("push failed")
		},
	}
	k8sClient := testutil.FakeClient(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		PutJSON("/namespaces/ns1/capps/app1", CappRequest{Name: "app1", Image: "nginx:2"})

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "GITOPS_SYNC_FAILED")

	live, err := getCapp(t, k8sClient)
	require.NoError(t, err)
	assert.Equal(t, "nginx:2", live.Spec.ConfigurationSpec.Template.Spec.Containers[0].Image,
		"the cluster write already happened; only git failed")
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

func TestDelete_GitFailure_ReturnsBadGateway(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{
		deleteFn: func(context.Context, string, string, string) (string, error) {
			return "", errors.New("push failed")
		},
	}
	k8sClient := testutil.FakeClient(t, gitSyncedCapp())

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Delete("/namespaces/ns1/capps/app1")

	assert.Equal(t, http.StatusBadGateway, w.Code)
	_, err := getCapp(t, k8sClient)
	assert.True(t, k8serrors.IsNotFound(err), "the capp is already deleted; only git failed")
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

func TestActingUser(t *testing.T) {
	tests := []struct {
		name string
		cred any
		want string
	}{
		{name: "openshift mode", cred: auth.ClusterCredential{ImpersonateUser: "alice"}, want: "alice"},
		{name: "passthrough mode", cred: auth.ClusterCredential{BearerToken: "tok"}, want: ""},
		{name: "no credential", cred: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, c := testutil.GinTestContext(t)
			if tt.cred != nil {
				c.Set(string(middleware.CredentialKey), tt.cred)
			}
			assert.Equal(t, tt.want, actingUser(c))
		})
	}
}

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
	h := New(false, nil, makeSizes())
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
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test1"}
	mock := &mockGitOpsSyncer{}

	w := syncEngine(t, mock, meta, makeSizes(), capp).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SyncResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "abc123", resp.CommitSHA)
	assert.NotEmpty(t, resp.Path)
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
	assert.True(t, k8s.HasBackupLabel(live.Labels), "label is applied first; re-sync retries the push")
}

func TestSync_PatchFailure_SkipsGit(t *testing.T) {
	meta := cluster.ClusterMeta{Name: "test"}
	mock := &mockGitOpsSyncer{}
	k8sClient := failWrites(t, makeCapp("app1", "ns1"))

	w := syncEngineWithClient(t, mock, meta, makeSizes(), k8sClient).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, mock.syncCalls)
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
	assert.False(t, k8s.HasBackupLabel(live.Labels), "label is removed first; only git failed")
}

func TestSync_SuccessVerifyLabel(t *testing.T) {
	capp := makeCapp("app1", "ns1")
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test1"}
	mock := &mockGitOpsSyncer{}

	k8sClient := testutil.FakeClient(t, capp)
	handler := New(true, mock, makeSizes())
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

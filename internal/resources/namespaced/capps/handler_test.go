package capps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New(false, nil, nil, makeSizes()))
}

// syncEngine creates an engine with gitops enabled, ClusterMeta in context,
// and a mock GitOpsSyncer.
func syncEngine(t *testing.T, mock *mockGitOpsSyncer, meta cluster.ClusterMeta, sizes config.CappSizes, objects ...client.Object) *testutil.EngineHelper {
	t.Helper()
	k8sClient := testutil.FakeClient(t, objects...)
	handler := New(true, mock, nil, sizes)
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
	capp := makeCapp("app1", "ns1")
	meta := cluster.ClusterMeta{Name: "test", GitOpsPath: "test1"}
	mock := &mockGitOpsSyncer{
		syncFn: func(_ context.Context, _, _, _ string, _ []byte) (string, error) {
			return "", errors.New("push failed")
		},
	}

	w := syncEngine(t, mock, meta, makeSizes(), capp).
		Post("/namespaces/ns1/capps/app1/sync", nil)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
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

	t.Run("bypass annotation set when hostname present", func(t *testing.T) {
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

		require.Equal(t, http.StatusOK, w.Code)
		var created cappv1alpha1.Capp
		require.NoError(t, targetClient.Get(context.Background(), client.ObjectKey{Namespace: "target-ns", Name: "my-app"}, &created))
		assert.Equal(t, "true", created.Annotations[migrationBypassAnnotation])
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
}

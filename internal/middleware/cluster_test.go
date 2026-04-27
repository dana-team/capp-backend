package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type mockClusterManager struct {
	getFn          func(name string) (*cluster.ClusterClient, error)
	clientForFn    func(*cluster.ClusterClient, auth.ClusterCredential) (client.Client, error)
	nsAllowedFn    func(*cluster.ClusterClient, string) bool
	isAnyHealthyFn func() bool
}

func (m *mockClusterManager) Get(name string) (*cluster.ClusterClient, error) {
	return m.getFn(name)
}
func (m *mockClusterManager) List() []cluster.ClusterMeta { return nil }
func (m *mockClusterManager) ClientFor(cc *cluster.ClusterClient, cred auth.ClusterCredential) (client.Client, error) {
	return m.clientForFn(cc, cred)
}
func (m *mockClusterManager) IsNamespaceAllowed(cc *cluster.ClusterClient, ns string) bool {
	return m.nsAllowedFn(cc, ns)
}
func (m *mockClusterManager) StartHealthChecks(context.Context, int) {}
func (m *mockClusterManager) IsAnyHealthy() bool {
	if m.isAnyHealthyFn != nil {
		return m.isAnyHealthyFn()
	}
	return true
}

func healthyCluster() *cluster.ClusterClient {
	return &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "prod", Healthy: true}}
}

func unhealthyCluster() *cluster.ClusterClient {
	return &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "prod", Healthy: false}}
}

func defaultMockClusterMgr() *mockClusterManager {
	return &mockClusterManager{
		getFn: func(_ string) (*cluster.ClusterClient, error) { return healthyCluster(), nil },
		clientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
			return nil, nil
		},
		nsAllowedFn: func(_ *cluster.ClusterClient, _ string) bool { return true },
	}
}

func serveClusterMiddleware(t *testing.T, mgr cluster.ClusterManager, path string, reqPath string, presets func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)

	if presets != nil {
		engine.Use(func(c *gin.Context) {
			presets(c)
			c.Next()
		})
	}

	engine.Use(Cluster(mgr))
	engine.GET(path, func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, reqPath, nil))
	return w
}

// -- Cluster middleware tests --

func TestCluster_EmptyClusterName_CallsNext(t *testing.T) {
	w := serveClusterMiddleware(t, defaultMockClusterMgr(), "/test", "/test", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCluster_ClusterNotFound_Returns404(t *testing.T) {
	mgr := defaultMockClusterMgr()
	mgr.getFn = func(_ string) (*cluster.ClusterClient, error) {
		return nil, cluster.ErrClusterNotFound
	}
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/test", "/clusters/missing/test",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{}) })
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCluster_GetOtherError_Returns500(t *testing.T) {
	mgr := defaultMockClusterMgr()
	mgr.getFn = func(_ string) (*cluster.ClusterClient, error) {
		return nil, errors.New("db error")
	}
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/test", "/clusters/prod/test",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{}) })
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCluster_UnhealthyCluster_Returns503(t *testing.T) {
	mgr := defaultMockClusterMgr()
	mgr.getFn = func(_ string) (*cluster.ClusterClient, error) { return unhealthyCluster(), nil }
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/test", "/clusters/prod/test",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{}) })
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCluster_NamespaceDenied_Returns403(t *testing.T) {
	mgr := defaultMockClusterMgr()
	mgr.nsAllowedFn = func(_ *cluster.ClusterClient, _ string) bool { return false }
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/namespaces/:namespace", "/clusters/prod/namespaces/denied-ns",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{}) })
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCluster_NamespaceAllowed_Passes(t *testing.T) {
	mgr := defaultMockClusterMgr()
	mgr.nsAllowedFn = func(_ *cluster.ClusterClient, _ string) bool { return true }
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/namespaces/:namespace", "/clusters/prod/namespaces/default",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{}) })
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCluster_MissingCredential_Returns500(t *testing.T) {
	w := serveClusterMiddleware(t, defaultMockClusterMgr(), "/clusters/:cluster/test", "/clusters/prod/test", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCluster_UserClientError_Returns500(t *testing.T) {
	mgr := defaultMockClusterMgr()
	callCount := 0
	mgr.clientForFn = func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
		callCount++
		if callCount == 1 {
			return nil, errors.New("client factory failure")
		}
		return nil, nil
	}
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/test", "/clusters/prod/test",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{BearerToken: "tok"}) })
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCluster_AdminClientError_Returns500(t *testing.T) {
	mgr := defaultMockClusterMgr()
	callCount := 0
	mgr.clientForFn = func(_ *cluster.ClusterClient, cred auth.ClusterCredential) (client.Client, error) {
		callCount++
		if callCount == 2 {
			return nil, errors.New("admin client failure")
		}
		return nil, nil
	}
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/test", "/clusters/prod/test",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{BearerToken: "tok"}) })
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCluster_Success_SetsAllContextKeys(t *testing.T) {
	mgr := defaultMockClusterMgr()
	var hasK8s, hasAdmin, hasMeta bool

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(func(c *gin.Context) {
		c.Set(string(CredentialKey), auth.ClusterCredential{BearerToken: "tok"})
		c.Next()
	})
	engine.Use(Cluster(mgr))
	engine.GET("/clusters/:cluster/test", func(c *gin.Context) {
		_, hasK8s = c.Get(string(K8sClientKey))
		_, hasAdmin = c.Get(string(AdminK8sClientKey))
		_, hasMeta = c.Get(string(ClusterMetaKey))
		c.Status(http.StatusOK)
	})
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/clusters/prod/test", nil))

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, hasK8s)
	assert.True(t, hasAdmin)
	assert.True(t, hasMeta)
}

func TestCluster_NoNamespaceParam_SkipsNamespaceCheck(t *testing.T) {
	mgr := defaultMockClusterMgr()
	nsCheckCalled := false
	mgr.nsAllowedFn = func(_ *cluster.ClusterClient, _ string) bool {
		nsCheckCalled = true
		return false
	}
	w := serveClusterMiddleware(t, mgr, "/clusters/:cluster/test", "/clusters/prod/test",
		func(c *gin.Context) { c.Set(string(CredentialKey), auth.ClusterCredential{}) })
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, nsCheckCalled)
}

func TestCluster_Success_SetsClusterMeta(t *testing.T) {
	mgr := defaultMockClusterMgr()
	var gotMeta cluster.ClusterMeta

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(func(c *gin.Context) {
		c.Set(string(CredentialKey), auth.ClusterCredential{})
		c.Next()
	})
	engine.Use(Cluster(mgr))
	engine.GET("/clusters/:cluster/test", func(c *gin.Context) {
		val, _ := c.Get(string(ClusterMetaKey))
		gotMeta = val.(cluster.ClusterMeta)
		c.Status(http.StatusOK)
	})
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/clusters/prod/test", nil))

	assert.Equal(t, "prod", gotMeta.Name)
	assert.True(t, gotMeta.Healthy)
}

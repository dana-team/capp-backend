package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newClusterClient(healthy bool) *cluster.ClusterClient {
	cc := &cluster.ClusterClient{
		Meta: cluster.ClusterMeta{Name: "prod"},
	}
	cc.SetHealthy(healthy)
	return cc
}

func TestCluster(t *testing.T) {
	cred := auth.ClusterCredential{BearerToken: "tok"}
	wantMeta := cluster.ClusterMeta{Name: "prod"}

	tests := []struct {
		name       string
		path       string
		route      string
		mgr        *testutil.MockClusterManager
		setCred    bool
		wantStatus int
		wantCode   string
	}{
		{
			name:       "skips when no cluster param",
			path:       "/test",
			route:      "/test",
			mgr:        &testutil.MockClusterManager{},
			wantStatus: http.StatusOK,
		},
		{
			name:  "returns 404 for unknown cluster",
			path:  "/clusters/unknown/test",
			route: "/clusters/:cluster/test",
			mgr: &testutil.MockClusterManager{
				GetFn: func(name string) (*cluster.ClusterClient, error) {
					return nil, cluster.ErrClusterNotFound
				},
			},
			setCred:    true,
			wantStatus: http.StatusNotFound,
			wantCode:   apierrors.CodeClusterNotFound,
		},
		{
			name:  "returns 500 for unexpected Get error",
			path:  "/clusters/prod/test",
			route: "/clusters/:cluster/test",
			mgr: &testutil.MockClusterManager{
				GetFn: func(name string) (*cluster.ClusterClient, error) {
					return nil, errors.New("db failure")
				},
			},
			setCred:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierrors.CodeInternal,
		},
		{
			name:  "returns 503 for unhealthy cluster",
			path:  "/clusters/prod/test",
			route: "/clusters/:cluster/test",
			mgr: &testutil.MockClusterManager{
				GetFn: func(name string) (*cluster.ClusterClient, error) {
					return newClusterClient(false), nil
				},
			},
			setCred:    true,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierrors.CodeClusterUnhealthy,
		},
		{
			name:  "returns 403 for denied namespace",
			path:  "/clusters/prod/namespaces/secret-ns/test",
			route: "/clusters/:cluster/namespaces/:namespace/test",
			mgr: &testutil.MockClusterManager{
				GetFn: func(name string) (*cluster.ClusterClient, error) {
					return newClusterClient(true), nil
				},
				IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, _ string) bool {
					return false
				},
			},
			setCred:    true,
			wantStatus: http.StatusForbidden,
			wantCode:   apierrors.CodeNamespaceDenied,
		},
		{
			name:  "returns 500 when credential missing from context",
			path:  "/clusters/prod/test",
			route: "/clusters/:cluster/test",
			mgr: &testutil.MockClusterManager{
				GetFn: func(name string) (*cluster.ClusterClient, error) {
					return newClusterClient(true), nil
				},
			},
			setCred:    false,
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierrors.CodeInternal,
		},
		{
			name:  "returns 500 when user ClientFor fails",
			path:  "/clusters/prod/test",
			route: "/clusters/:cluster/test",
			mgr: &testutil.MockClusterManager{
				GetFn: func(name string) (*cluster.ClusterClient, error) {
					return newClusterClient(true), nil
				},
				ClientForFn: func(_ *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
					return nil, errors.New("bad config")
				},
			},
			setCred:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierrors.CodeInternal,
		},
		{
			name:  "returns 500 when admin ClientFor fails",
			path:  "/clusters/prod/test",
			route: "/clusters/:cluster/test",
			mgr: &testutil.MockClusterManager{
				GetFn: func(name string) (*cluster.ClusterClient, error) {
					cc := newClusterClient(true)
					cc.Meta = wantMeta
					return cc, nil
				},
				ClientForFn: func(_ *cluster.ClusterClient, c auth.ClusterCredential) (client.Client, error) {
					if c.BearerToken == "" {
						return nil, errors.New("admin config broken")
					}
					return testutil.FakeClient(t), nil
				},
			},
			setCred:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierrors.CodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, engine := gin.CreateTestContext(w)

			if tt.setCred {
				engine.Use(func(c *gin.Context) {
					c.Set(string(middleware.CredentialKey), cred)
					c.Next()
				})
			}
			engine.Use(middleware.Cluster(tt.mgr))
			engine.GET(tt.route, func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			engine.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
			if tt.wantCode != "" {
				apiErr := decodeError(t, w)
				assert.Equal(t, tt.wantCode, apiErr.Code)
			}
		})
	}
}

func TestClusterSuccess(t *testing.T) {
	cred := auth.ClusterCredential{BearerToken: "tok"}
	userClient := testutil.FakeClient(t)
	adminClient := testutil.FakeClient(t)
	wantMeta := cluster.ClusterMeta{Name: "prod"}

	callCount := 0
	mgr := &testutil.MockClusterManager{
		GetFn: func(name string) (*cluster.ClusterClient, error) {
			cc := newClusterClient(true)
			cc.Meta = wantMeta
			return cc, nil
		},
		ClientForFn: func(_ *cluster.ClusterClient, c auth.ClusterCredential) (client.Client, error) {
			callCount++
			if c.BearerToken != "" {
				return userClient, nil
			}
			return adminClient, nil
		},
	}

	var gotUser, gotAdmin client.Client
	var gotMeta cluster.ClusterMeta

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.CredentialKey), cred)
		c.Next()
	})
	engine.Use(middleware.Cluster(mgr))
	engine.GET("/clusters/:cluster/test", func(c *gin.Context) {
		val, _ := c.Get(string(middleware.K8sClientKey))
		gotUser = val.(client.Client)
		val, _ = c.Get(string(middleware.AdminK8sClientKey))
		gotAdmin = val.(client.Client)
		val, _ = c.Get(string(middleware.ClusterMetaKey))
		gotMeta = val.(cluster.ClusterMeta)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/clusters/prod/test", nil)
	engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, userClient, gotUser)
	assert.Equal(t, adminClient, gotAdmin)
	assert.Equal(t, wantMeta, gotMeta)
	assert.Equal(t, 2, callCount, "ClientFor called for user and admin")
}

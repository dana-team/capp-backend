package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Port:                8080,
			ReadTimeoutSeconds:  30,
			WriteTimeoutSeconds: 30,
			IdleTimeoutSeconds:  60,
			CORSAllowedOrigins: []string{"*"},
		},
		Auth: config.AuthConfig{
			Mode: "passthrough",
			RateLimit: config.RateLimitConfig{
				Enabled:           false,
				RequestsPerSecond: 20,
				Burst:             40,
			},
		},
		Metrics: config.MetricsConfig{Enabled: true, Path: "/metrics"},
	}
}

func testServer(t *testing.T, cfg *config.Config, clusterMgr *testutil.MockClusterManager) *Server {
	t.Helper()
	if clusterMgr == nil {
		clusterMgr = &testutil.MockClusterManager{
			IsAnyHealthyFn: func() bool { return true },
			ListFn:         func() []cluster.ClusterMeta { return nil },
		}
	}
	authMgr := &testutil.MockAuthManager{}
	registry := resources.NewRegistry(map[string]bool{})
	logger, _ := zap.NewDevelopment()
	return New(cfg, authMgr, clusterMgr, registry, logger)
}

func serveRequest(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w
}

// -- Health and readiness tests --

func TestHealthz_Returns200(t *testing.T) {
	srv := testServer(t, testConfig(), nil)
	w := serveRequest(t, srv, http.MethodGet, "/healthz")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReadyz_HealthyClusters_Returns200(t *testing.T) {
	mgr := &testutil.MockClusterManager{
		IsAnyHealthyFn: func() bool { return true },
		ListFn:         func() []cluster.ClusterMeta { return nil },
	}
	srv := testServer(t, testConfig(), mgr)
	w := serveRequest(t, srv, http.MethodGet, "/readyz")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReadyz_NoHealthyClusters_Returns503(t *testing.T) {
	mgr := &testutil.MockClusterManager{
		IsAnyHealthyFn: func() bool { return false },
		ListFn:         func() []cluster.ClusterMeta { return nil },
	}
	srv := testServer(t, testConfig(), mgr)
	w := serveRequest(t, srv, http.MethodGet, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// -- Cluster listing tests --

func TestListClusters_ReturnsAll(t *testing.T) {
	mgr := &testutil.MockClusterManager{
		IsAnyHealthyFn: func() bool { return true },
		ListFn: func() []cluster.ClusterMeta {
			return []cluster.ClusterMeta{
				{Name: "prod", Healthy: true},
				{Name: "staging", Healthy: false},
			}
		},
	}
	srv := testServer(t, testConfig(), mgr)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	srv.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items, ok := body["items"].([]any)
	require.True(t, ok)
	assert.Len(t, items, 2)
}

func TestGetCluster_Found(t *testing.T) {
	mgr := &testutil.MockClusterManager{
		IsAnyHealthyFn: func() bool { return true },
		ListFn: func() []cluster.ClusterMeta {
			return []cluster.ClusterMeta{{Name: "prod", DisplayName: "Production", Healthy: true}}
		},
	}
	srv := testServer(t, testConfig(), mgr)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/prod", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	srv.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "prod", body["name"])
}

func TestGetCluster_NotFound(t *testing.T) {
	mgr := &testutil.MockClusterManager{
		IsAnyHealthyFn: func() bool { return true },
		ListFn:         func() []cluster.ClusterMeta { return nil },
	}
	srv := testServer(t, testConfig(), mgr)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/missing", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	srv.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Rate limit tests --

func TestNew_RateLimitEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Auth.RateLimit.Enabled = true
	cfg.Auth.RateLimit.RequestsPerSecond = 0.001
	cfg.Auth.RateLimit.Burst = 1

	srv := testServer(t, cfg, nil)

	w1 := serveRequest(t, srv, http.MethodGet, "/healthz")
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := serveRequest(t, srv, http.MethodGet, "/healthz")
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
}

func TestNew_RateLimitDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Auth.RateLimit.Enabled = false

	srv := testServer(t, cfg, nil)
	for i := 0; i < 10; i++ {
		w := serveRequest(t, srv, http.MethodGet, "/healthz")
		assert.Equal(t, http.StatusOK, w.Code)
	}
}

// -- Metrics endpoint tests --

func TestNew_MetricsEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.Enabled = true
	cfg.Metrics.Path = "/metrics"

	srv := testServer(t, cfg, nil)
	w := serveRequest(t, srv, http.MethodGet, "/metrics")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestNew_MetricsDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.Enabled = false

	srv := testServer(t, cfg, nil)
	w := serveRequest(t, srv, http.MethodGet, "/metrics")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

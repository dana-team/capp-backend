package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func cfuncServer(t *testing.T, enabled bool) http.Handler {
	t.Helper()
	cfg := &config.Config{Auth: config.AuthConfig{Mode: config.AuthModePassthrough}}
	cfg.Resources.Cfunc = config.CfuncConfig{
		Enabled:        enabled,
		RegistryPrefix: "registry.example/fission",
		Sizes: map[string]config.ResourceSize{
			"small": {
				Requests: config.ResourceQuantities{CPU: "100m", Memory: "128Mi"},
				Limits:   config.ResourceQuantities{CPU: "200m", Memory: "256Mi"},
			},
			"large": {
				Requests: config.ResourceQuantities{CPU: "500m", Memory: "512Mi"},
				Limits:   config.ResourceQuantities{CPU: "1", Memory: "1Gi"},
			},
		},
		Flavors: map[string]config.CfuncFlavor{
			"python": {Image: "python-env:1.35.0", BuildImage: "python-builder:1.35.0"},
			"java":   {Image: "jvm-env:1.32.0", KeepArchive: true},
		},
	}
	s, err := New(cfg, "dev", &testutil.MockAuthManager{}, &testutil.MockClusterManager{}, &resources.Registry{}, zap.NewNop())
	require.NoError(t, err)
	return s.httpServer.Handler
}

func TestCfuncNameRoutes(t *testing.T) {
	handler := cfuncServer(t, true)
	for path, want := range map[string]string{
		"/api/v1/cfunc/flavors": `{"items":["java","python"],"total":2}`,
		"/api/v1/cfunc/sizes":   `{"items":["large","small"],"total":2}`,
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

		require.Equal(t, http.StatusOK, w.Code, path)
		// Exact match: the response must carry names only, with no image,
		// registry or resource value behind them.
		assert.JSONEq(t, want, w.Body.String(), path)
	}
}

func TestCfuncNameRoutes_AbsentWhenDisabled(t *testing.T) {
	handler := cfuncServer(t, false)
	for _, path := range []string{"/api/v1/cfunc/flavors", "/api/v1/cfunc/sizes"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
}

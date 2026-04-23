package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func serveWithLogging(t *testing.T, handler gin.HandlerFunc, presets func(*gin.Context)) (*httptest.ResponseRecorder, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	if presets != nil {
		engine.Use(func(c *gin.Context) { presets(c); c.Next() })
	}
	engine.Use(Logging(logger))
	engine.GET("/test", handler)
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	return w, logs
}

// -- Logging middleware tests --

func TestLogging_InfoOnSuccess(t *testing.T) {
	_, logs := serveWithLogging(t, func(c *gin.Context) {
		c.Status(http.StatusOK)
	}, nil)

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zapcore.InfoLevel, logs.All()[0].Level)
}

func TestLogging_WarnOn4xx(t *testing.T) {
	_, logs := serveWithLogging(t, func(c *gin.Context) {
		c.Status(http.StatusBadRequest)
	}, nil)

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zapcore.WarnLevel, logs.All()[0].Level)
}

func TestLogging_ErrorOn5xx(t *testing.T) {
	_, logs := serveWithLogging(t, func(c *gin.Context) {
		c.Status(http.StatusInternalServerError)
	}, nil)

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, zapcore.ErrorLevel, logs.All()[0].Level)
}

func TestLogging_IncludesClusterField(t *testing.T) {
	_, logs := serveWithLogging(t, func(c *gin.Context) {
		c.Status(http.StatusOK)
	}, func(c *gin.Context) {
		c.Set(string(ClusterMetaKey), cluster.ClusterMeta{Name: "prod"})
	})

	require.Equal(t, 1, logs.Len())
	fields := logs.All()[0].ContextMap()
	assert.Equal(t, "prod", fields["cluster"])
}

func TestLogging_OmitsClusterField(t *testing.T) {
	_, logs := serveWithLogging(t, func(c *gin.Context) {
		c.Status(http.StatusOK)
	}, nil)

	require.Equal(t, 1, logs.Len())
	fields := logs.All()[0].ContextMap()
	_, hasCluster := fields["cluster"]
	assert.False(t, hasCluster)
}

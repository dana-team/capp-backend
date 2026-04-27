package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// -- Recovery middleware tests --

func TestRecovery_PanicReturns500(t *testing.T) {
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Recovery(logger))
	engine.GET("/test", func(_ *gin.Context) { panic("boom") })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "INTERNAL_ERROR", errObj["code"])
}

func TestRecovery_PanicLogsError(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Recovery(logger))
	engine.GET("/test", func(_ *gin.Context) { panic("boom") })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	require.GreaterOrEqual(t, logs.Len(), 1)
	assert.Equal(t, "panic recovered", logs.All()[0].Message)
}

func TestRecovery_NoPanic_PassesThrough(t *testing.T) {
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Recovery(logger))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	assert.Equal(t, http.StatusOK, w.Code)
}

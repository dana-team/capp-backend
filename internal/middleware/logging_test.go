package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestLogging_UnwrapsErrorCause verifies that an APIError's sanitized cause
// (never exposed to the client, and excluded from APIError.Error()) still
// reaches the server logs, since the logging middleware is the only place
// that error is ever recorded.
func TestLogging_UnwrapsErrorCause(t *testing.T) {
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	core, logs := observer.New(zap.DebugLevel)
	engine.Use(middleware.Logging(zap.New(core)))
	engine.GET("/metrics", func(c *gin.Context) {
		apierrors.Respond(c, apierrors.NewPrometheusQueryFailed(errors.New("connection refused")))
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	engine.ServeHTTP(w, req)

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.NotContains(t, entry.ContextMap()["error"], "connection refused",
		"the client-facing error message must not carry the cause")
	assert.Contains(t, entry.ContextMap()["errorCause"], "connection refused")
}

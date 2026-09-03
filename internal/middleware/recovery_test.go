package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestRecovery(t *testing.T) {
	logger := zap.NewNop()

	t.Run("returns 500 with INTERNAL_ERROR on panic", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, engine := gin.CreateTestContext(w)
		engine.Use(middleware.Recovery(logger))
		engine.GET("/test", func(_ *gin.Context) { panic("something broke") })

		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		apiErr := decodeError(t, w)
		assert.Equal(t, "INTERNAL_ERROR", apiErr.Code)
		assert.NotEmpty(t, apiErr.Message)
	})

	t.Run("passes through when handler does not panic", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, engine := gin.CreateTestContext(w)
		engine.Use(middleware.Recovery(logger))
		engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

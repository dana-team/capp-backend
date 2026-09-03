package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestCORS(t *testing.T) {
	t.Run("sets origin header for matched origin", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, engine := gin.CreateTestContext(w)
		engine.Use(middleware.CORS([]string{"https://app.example.com"}))
		engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Origin", "https://app.example.com")
		engine.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "Origin", w.Header().Get("Vary"))
	})

	t.Run("wildcard without origin header sets allow-origin to *", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, engine := gin.CreateTestContext(w)
		engine.Use(middleware.CORS([]string{"*"}))
		engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		engine.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("unmatched origin omits allow-origin but sets other headers", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, engine := gin.CreateTestContext(w)
		engine.Use(middleware.CORS([]string{"https://app.example.com"}))
		engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		engine.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
		assert.NotEmpty(t, w.Header().Get("Access-Control-Allow-Methods"))
		assert.NotEmpty(t, w.Header().Get("Access-Control-Allow-Headers"))
		assert.Equal(t, "86400", w.Header().Get("Access-Control-Max-Age"))
	})

	t.Run("OPTIONS preflight returns 204 with no body", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, engine := gin.CreateTestContext(w)
		engine.Use(middleware.CORS([]string{"https://app.example.com"}))
		engine.OPTIONS("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

		req := httptest.NewRequest(http.MethodOptions, "/test", nil)
		req.Header.Set("Origin", "https://app.example.com")
		engine.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNoContent, w.Code)
		assert.Empty(t, w.Body.String())
	})
}

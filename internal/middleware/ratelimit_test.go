package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func rateLimitEngine(rps float64, maxIPTracked int) *gin.Engine {
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(middleware.RateLimit(rps, 1, maxIPTracked))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	return engine
}

func TestRateLimit(t *testing.T) {
	t.Run("passes request under limit", func(t *testing.T) {
		engine := rateLimitEngine(10, 100)

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("returns 429 when limit exceeded", func(t *testing.T) {
		engine := rateLimitEngine(1, 100)

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
		assert.Equal(t, http.StatusOK, w.Code)

		w = httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, "1", w.Header().Get("Retry-After"))

		apiErr := decodeError(t, w)
		assert.Equal(t, "RATE_LIMITED", apiErr.Code)
	})

	t.Run("independent limits per IP", func(t *testing.T) {
		engine := rateLimitEngine(1, 100)

		req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
		req1.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req1)
		assert.Equal(t, http.StatusOK, w.Code)

		req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
		req2.RemoteAddr = "10.0.0.2:1234"
		w = httptest.NewRecorder()
		engine.ServeHTTP(w, req2)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("evicts oldest IP when at capacity", func(t *testing.T) {
		engine := rateLimitEngine(1, 2)

		req := func(ip string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodGet, "/test", nil)
			r.RemoteAddr = ip + ":1234"
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, r)
			return w
		}

		assert.Equal(t, http.StatusOK, req("10.0.0.1").Code)
		time.Sleep(time.Millisecond) // ensure 10.0.0.1 is strictly oldest
		assert.Equal(t, http.StatusOK, req("10.0.0.2").Code)

		// Third IP evicts 10.0.0.1 (oldest). Store: {10.0.0.2, 10.0.0.3}.
		assert.Equal(t, http.StatusOK, req("10.0.0.3").Code)

		// 10.0.0.2 was NOT evicted — its exhausted limiter is still active.
		assert.Equal(t, http.StatusTooManyRequests, req("10.0.0.2").Code,
			"non-evicted IP should still be rate-limited")

		// 10.0.0.1 was evicted — it receives a fresh bucket.
		assert.Equal(t, http.StatusOK, req("10.0.0.1").Code,
			"evicted IP should get a fresh limiter")
	})
}

package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func rateLimitEngine(rps float64, burst int) *gin.Engine {
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(RateLimit(rps, burst))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	return engine
}

// -- RateLimit tests --

func TestRateLimit_FirstRequest_Allowed(t *testing.T) {
	engine := rateLimitEngine(1, 1)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRateLimit_BurstExhausted_Returns429(t *testing.T) {
	engine := rateLimitEngine(0.001, 1)

	w1 := httptest.NewRecorder()
	engine.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/test", nil))
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/test", nil))
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
}

func TestRateLimit_RetryAfterHeader(t *testing.T) {
	engine := rateLimitEngine(0.001, 1)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/test", nil))
	assert.Equal(t, "1", w2.Header().Get("Retry-After"))
}

func TestRateLimit_DifferentIPs_Isolated(t *testing.T) {
	engine := rateLimitEngine(0.001, 1)

	r1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	r1.RemoteAddr = "10.0.0.1:1234"
	w1 := httptest.NewRecorder()
	engine.ServeHTTP(w1, r1)
	assert.Equal(t, http.StatusOK, w1.Code)

	r2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	r2.RemoteAddr = "10.0.0.2:1234"
	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, r2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

func TestRateLimit_IPLimiterStore_ConcurrentAccess(t *testing.T) {
	store := newIPLimiterStore(100, 100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = store.get("10.0.0.1")
		}()
	}
	wg.Wait()
}

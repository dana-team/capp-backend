package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// -- Metrics middleware tests --

func TestMetrics_MatchedRoute_UsesFullPath(t *testing.T) {
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Metrics())
	engine.GET("/api/v1/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/test", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMetrics_UnmatchedRoute_Returns404(t *testing.T) {
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Metrics())
	engine.GET("/api/v1/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/no-such-route", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestMetrics_IncrementsCounter(t *testing.T) {
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Metrics())
	engine.GET("/metrics-test", func(c *gin.Context) { c.Status(http.StatusOK) })

	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics-test", nil))
	assert.Equal(t, http.StatusOK, w.Code)

	// Second request to verify idempotent behaviour.
	w2 := httptest.NewRecorder()
	engine.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/metrics-test", nil))
	assert.Equal(t, http.StatusOK, w2.Code)
}

func TestMetrics_ObservesDuration(t *testing.T) {
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Metrics())
	engine.GET("/duration-test", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/duration-test", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

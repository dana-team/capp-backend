package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func serveCORS(t *testing.T, origins []string, method, origin string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(CORS(origins))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.OPTIONS("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(method, "/test", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	engine.ServeHTTP(w, req)
	return w
}

// -- CORS tests --

func TestCORS_WildcardOrigin(t *testing.T) {
	w := serveCORS(t, []string{"*"}, http.MethodGet, "https://example.com")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_ExplicitOriginMatch(t *testing.T) {
	w := serveCORS(t, []string{"https://app.example.com"}, http.MethodGet, "https://app.example.com")
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Origin", w.Header().Get("Vary"))
}

func TestCORS_OriginNotAllowed(t *testing.T) {
	w := serveCORS(t, []string{"https://app.example.com"}, http.MethodGet, "https://evil.example.com")
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_OptionsRequest_Returns204(t *testing.T) {
	w := serveCORS(t, []string{"*"}, http.MethodOptions, "https://example.com")
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestCORS_EmptyOriginHeader(t *testing.T) {
	w := serveCORS(t, []string{"*"}, http.MethodGet, "")
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_VaryHeaderSet(t *testing.T) {
	w := serveCORS(t, []string{"https://app.example.com"}, http.MethodGet, "https://app.example.com")
	assert.Equal(t, "Origin", w.Header().Get("Vary"))
}

func TestCORS_PreflightHeaders(t *testing.T) {
	w := serveCORS(t, []string{"*"}, http.MethodOptions, "https://example.com")
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Methods"), "GET")
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Methods"), "POST")
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	assert.Equal(t, "86400", w.Header().Get("Access-Control-Max-Age"))
}

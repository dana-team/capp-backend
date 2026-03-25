package apierrors

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// recordResponse runs handler h with a fresh Gin context and returns the
// recorder so tests can inspect status and body.
func recordResponse(t *testing.T, h gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h(c)
	return w
}

// decodeError extracts the "error" field from the response body.
func decodeError(t *testing.T, w *httptest.ResponseRecorder) APIError {
	t.Helper()
	var envelope struct {
		Err APIError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&envelope))
	return envelope.Err
}

// ── Constructor tests ─────────────────────────────────────────────────────────

func TestNewNotFound(t *testing.T) {
	e := NewNotFound("capp", "my-app")
	assert.Equal(t, CodeNotFound, e.Code)
	assert.Equal(t, http.StatusNotFound, e.Status)
	assert.Contains(t, e.Message, "my-app")
}

func TestNewConflict(t *testing.T) {
	e := NewConflict("capp", "my-app")
	assert.Equal(t, CodeConflict, e.Code)
	assert.Equal(t, http.StatusConflict, e.Status)
}

func TestNewForbidden(t *testing.T) {
	e := NewForbidden("not allowed")
	assert.Equal(t, CodeForbidden, e.Code)
	assert.Equal(t, http.StatusForbidden, e.Status)
}

func TestNewUnauthorized(t *testing.T) {
	e := NewUnauthorized("no token")
	assert.Equal(t, CodeUnauthorized, e.Code)
	assert.Equal(t, http.StatusUnauthorized, e.Status)
}

func TestNewBadRequest(t *testing.T) {
	e := NewBadRequest("invalid input")
	assert.Equal(t, CodeBadRequest, e.Code)
	assert.Equal(t, http.StatusBadRequest, e.Status)
}

func TestNewInternal(t *testing.T) {
	e := NewInternal(errors.New("db gone"))
	assert.Equal(t, CodeInternal, e.Code)
	assert.Equal(t, http.StatusInternalServerError, e.Status)
	assert.Contains(t, e.Message, "db gone")
}

func TestNewClusterNotFound(t *testing.T) {
	e := NewClusterNotFound("staging")
	assert.Equal(t, CodeClusterNotFound, e.Code)
	assert.Equal(t, http.StatusNotFound, e.Status)
	assert.Contains(t, e.Message, "staging")
}

func TestNewClusterUnhealthy(t *testing.T) {
	e := NewClusterUnhealthy("prod")
	assert.Equal(t, CodeClusterUnhealthy, e.Code)
	assert.Equal(t, http.StatusServiceUnavailable, e.Status)
}

func TestNewNotSupported(t *testing.T) {
	e := NewNotSupported("Login")
	assert.Equal(t, CodeNotSupported, e.Code)
	assert.Equal(t, http.StatusNotImplemented, e.Status)
}

func TestAPIError_ErrorString(t *testing.T) {
	e := NewNotFound("capp", "foo")
	assert.Contains(t, e.Error(), CodeNotFound)
	assert.Contains(t, e.Error(), "foo")
}

// ── Respond tests ─────────────────────────────────────────────────────────────

func TestRespond_APIError(t *testing.T) {
	w := recordResponse(t, func(c *gin.Context) {
		Respond(c, NewForbidden("nope"))
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	e := decodeError(t, w)
	assert.Equal(t, CodeForbidden, e.Code)
}

func TestRespond_K8sNotFound(t *testing.T) {
	gr := schema.GroupResource{Group: "rcs.dana.io", Resource: "capps"}
	k8sErr := k8serrors.NewNotFound(gr, "my-app")

	w := recordResponse(t, func(c *gin.Context) {
		Respond(c, k8sErr)
	})
	assert.Equal(t, http.StatusNotFound, w.Code)
	e := decodeError(t, w)
	assert.Equal(t, CodeNotFound, e.Code)
}

func TestRespond_K8sAlreadyExists(t *testing.T) {
	gr := schema.GroupResource{Resource: "capps"}
	w := recordResponse(t, func(c *gin.Context) {
		Respond(c, k8serrors.NewAlreadyExists(gr, "app"))
	})
	assert.Equal(t, http.StatusConflict, w.Code)
	e := decodeError(t, w)
	assert.Equal(t, CodeConflict, e.Code)
}

func TestRespond_K8sForbidden(t *testing.T) {
	w := recordResponse(t, func(c *gin.Context) {
		Respond(c, k8serrors.NewForbidden(schema.GroupResource{}, "x", fmt.Errorf("denied")))
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	e := decodeError(t, w)
	assert.Equal(t, CodeForbidden, e.Code)
}

func TestRespond_GenericError(t *testing.T) {
	w := recordResponse(t, func(c *gin.Context) {
		Respond(c, errors.New("something exploded"))
	})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	e := decodeError(t, w)
	assert.Equal(t, CodeInternal, e.Code)
}

func TestRespond_Aborts(t *testing.T) {
	// Respond must call c.Abort() so subsequent handlers are not called.
	called := false
	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	engine.GET("/", func(c *gin.Context) {
		Respond(c, NewNotFound("x", "y"))
	}, func(c *gin.Context) {
		called = true
	})
	engine.ServeHTTP(w, c.Request)
	assert.False(t, called, "handler after Respond should not be called")
}

func TestRespondOK(t *testing.T) {
	w := recordResponse(t, func(c *gin.Context) {
		RespondOK(c, gin.H{"key": "value"})
	})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRespondCreated(t *testing.T) {
	w := recordResponse(t, func(c *gin.Context) {
		RespondCreated(c, gin.H{"name": "new-app"})
	})
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestRespondNoContent(t *testing.T) {
	w := recordResponse(t, func(c *gin.Context) {
		RespondNoContent(c)
	})
	assert.Equal(t, http.StatusNoContent, w.Code)
}

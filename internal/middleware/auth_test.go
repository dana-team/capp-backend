package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type mockAuthManager struct {
	authenticateFn func(ctx context.Context, clusterName string, r *http.Request) (auth.ClusterCredential, error)
}

func (m *mockAuthManager) Authenticate(ctx context.Context, clusterName string, r *http.Request) (auth.ClusterCredential, error) {
	return m.authenticateFn(ctx, clusterName, r)
}
func (m *mockAuthManager) Login(context.Context, string, string) (auth.TokenPair, error) {
	return auth.TokenPair{}, nil
}
func (m *mockAuthManager) PasswordLogin(context.Context, string, string) (auth.TokenPair, error) {
	return auth.TokenPair{}, nil
}
func (m *mockAuthManager) Refresh(context.Context, string) (auth.TokenPair, error) {
	return auth.TokenPair{}, nil
}

func runAuthMiddleware(t *testing.T, mgr auth.AuthManager) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Auth(mgr))
	engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	return w
}

// -- Auth middleware tests --

func TestAuth_Success_SetsCredential(t *testing.T) {
	expectedCred := auth.ClusterCredential{BearerToken: "tok-123"}
	var capturedCred auth.ClusterCredential

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Auth(&mockAuthManager{
		authenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
			return expectedCred, nil
		},
	}))
	engine.GET("/test", func(c *gin.Context) {
		val, exists := c.Get(string(CredentialKey))
		require.True(t, exists)
		capturedCred = val.(auth.ClusterCredential)
		c.Status(http.StatusOK)
	})
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, expectedCred.BearerToken, capturedCred.BearerToken)
}

func TestAuth_ErrUnauthenticated_Returns401(t *testing.T) {
	w := runAuthMiddleware(t, &mockAuthManager{
		authenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
			return auth.ClusterCredential{}, auth.ErrUnauthenticated
		},
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_ErrTokenExpired_Returns401(t *testing.T) {
	w := runAuthMiddleware(t, &mockAuthManager{
		authenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
			return auth.ClusterCredential{}, auth.ErrTokenExpired
		},
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_OtherError_Returns500(t *testing.T) {
	w := runAuthMiddleware(t, &mockAuthManager{
		authenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
			return auth.ClusterCredential{}, errors.New("unexpected failure")
		},
	})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAuth_CallsNextOnSuccess(t *testing.T) {
	nextCalled := false
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Auth(&mockAuthManager{
		authenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
			return auth.ClusterCredential{}, nil
		},
	}))
	engine.GET("/test", func(c *gin.Context) {
		nextCalled = true
		c.Status(http.StatusOK)
	})
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	assert.True(t, nextCalled)
}

func TestAuth_AbortsOnError(t *testing.T) {
	nextCalled := false
	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)
	engine.Use(Auth(&mockAuthManager{
		authenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
			return auth.ClusterCredential{}, auth.ErrUnauthenticated
		},
	}))
	engine.GET("/test", func(_ *gin.Context) { nextCalled = true })
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	assert.False(t, nextCalled)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body, "error")
}

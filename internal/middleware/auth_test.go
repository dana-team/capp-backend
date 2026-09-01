package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) apierrors.APIError {
	t.Helper()
	var envelope struct {
		Err apierrors.APIError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&envelope))
	return envelope.Err
}

func TestAuth(t *testing.T) {
	wantCred := auth.ClusterCredential{BearerToken: "tok-123"}

	tests := []struct {
		name       string
		mgr        *testutil.MockAuthManager
		wantStatus int
		wantCode   string
		wantCred   *auth.ClusterCredential
	}{
		{
			name: "attaches credential on success",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return wantCred, nil
				},
			},
			wantStatus: http.StatusOK,
			wantCred:   &wantCred,
		},
		{
			name: "returns 401 for unauthenticated error",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, auth.ErrUnauthenticated
				},
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   apierrors.CodeUnauthorized,
		},
		{
			name: "returns 401 for token expired error",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, auth.ErrTokenExpired
				},
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   apierrors.CodeUnauthorized,
		},
		{
			name: "returns 500 for unexpected error",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, errors.New("connection refused")
				},
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierrors.CodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedCred auth.ClusterCredential
			var credSet bool

			w := httptest.NewRecorder()
			_, engine := gin.CreateTestContext(w)
			engine.Use(middleware.Auth(tt.mgr))
			engine.GET("/clusters/:cluster/test", func(c *gin.Context) {
				val, exists := c.Get(string(middleware.CredentialKey))
				if exists {
					capturedCred = val.(auth.ClusterCredential)
					credSet = true
				}
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/clusters/my-cluster/test", nil)
			engine.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)

			if tt.wantCred != nil {
				require.True(t, credSet, "credential must be set in context")
				assert.Equal(t, *tt.wantCred, capturedCred)
			}
			if tt.wantCode != "" {
				apiErr := decodeError(t, w)
				assert.Equal(t, tt.wantCode, apiErr.Code)
			}
		})
	}
}

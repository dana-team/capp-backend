package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	authenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
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
	wantCred := auth.ClusterCredential{ImpersonateUser: "alice"}
	ldapCred := auth.ClusterCredential{
		ImpersonateUser: "Alice", ImpersonateGroups: []string{"Developers"},
		AuthProvider: "ldap", IdentityAuthority: "capp",
	}

	tests := []struct {
		name        string
		mgr         *testutil.MockAuthManager
		wantStatus  int
		wantCode    string
		wantMessage string
		wantCred    *auth.ClusterCredential
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
			name: "attaches LDAP impersonation credentials",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return ldapCred, nil
				},
			},
			wantStatus: http.StatusOK,
			wantCred:   &ldapCred,
		},
		{
			name: "returns 401 for unauthenticated error",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, fmt.Errorf("private diagnostic: %w", auth.ErrUnauthenticated)
				},
			},
			wantStatus:  http.StatusUnauthorized,
			wantCode:    apierrors.CodeUnauthorized,
			wantMessage: auth.ErrUnauthenticated.Error(),
		},
		{
			name: "returns 401 for token expired error",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, fmt.Errorf("private diagnostic: %w", auth.ErrTokenExpired)
				},
			},
			wantStatus:  http.StatusUnauthorized,
			wantCode:    apierrors.CodeUnauthorized,
			wantMessage: auth.ErrTokenExpired.Error(),
		},
		{
			name: "returns 503 for directory failure",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, fmt.Errorf("private diagnostic: %w", auth.ErrDirectoryUnavailable)
				},
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    apierrors.CodeInternal,
			wantMessage: auth.ErrDirectoryUnavailable.Error(),
		},
		{
			name: "returns 503 for unavailable identity verification",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, fmt.Errorf("private diagnostic: %w", auth.ErrIdentityUnavailable)
				},
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    apierrors.CodeInternal,
			wantMessage: auth.ErrIdentityUnavailable.Error(),
		},
		{
			name: "returns 500 for unexpected error",
			mgr: &testutil.MockAuthManager{
				AuthenticateFn: func(_ context.Context, _ string, _ *http.Request) (auth.ClusterCredential, error) {
					return auth.ClusterCredential{}, errors.New("private diagnostic")
				},
			},
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apierrors.CodeInternal,
			wantMessage: "internal server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedCred auth.ClusterCredential
			var credSet bool
			var handlerCalled bool

			w := httptest.NewRecorder()
			_, engine := gin.CreateTestContext(w)
			core, logs := observer.New(zap.DebugLevel)
			engine.Use(middleware.Logging(zap.New(core)))
			engine.Use(func(c *gin.Context) {
				if _, ok := middleware.Authenticate(c, tt.mgr); ok {
					c.Next()
				}
			})
			engine.GET("/clusters/:cluster/test", func(c *gin.Context) {
				handlerCalled = true
				val, exists := c.Get(string(middleware.CredentialKey))
				if exists {
					capturedCred, _ = val.(auth.ClusterCredential)
					credSet = true
				}
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/clusters/my-cluster/test", nil)
			req.Header.Set("Authorization", "Bearer private-token")
			engine.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)

			if tt.wantCred != nil {
				require.True(t, credSet, "credential must be set in context")
				assert.Equal(t, *tt.wantCred, capturedCred)
			}
			if tt.wantCode != "" {
				if handlerCalled {
					t.Fatal("Auth() invoked handler after authentication failure")
				}
				apiErr := decodeError(t, w)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				if apiErr.Message != tt.wantMessage || apiErr.Status != tt.wantStatus {
					t.Errorf("Auth() error = %+v, want message %q and status %d", apiErr, tt.wantMessage, tt.wantStatus)
				}
			}
			if logs.Len() != 1 {
				t.Fatalf("Auth() logged %d requests, want 1", logs.Len())
			}
			for _, secret := range []string{"private diagnostic", "private-token"} {
				if strings.Contains(fmt.Sprint(logs.All()), secret) {
					t.Errorf("Auth() logged sensitive value %q", secret)
				}
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool
		err    error
	}{
		{name: "canceled before authentication", before: true},
		{name: "canceled during successful authentication"},
		{name: "canceled during unavailable identity review", err: auth.ErrIdentityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.before {
				cancel()
			}
			called := false
			mgr := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
				called = true
				cancel()
				return auth.ClusterCredential{ImpersonateUser: "alice"}, tc.err
			}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			_, ok := middleware.Authenticate(c, mgr)
			require.False(t, ok)
			require.True(t, c.IsAborted())
			require.Equal(t, !tc.before, called)
			require.Equal(t, middleware.StatusClientClosedRequest, w.Code)
			require.Empty(t, w.Body.String())
			require.Empty(t, c.Errors)
			_, attached := c.Get(string(middleware.CredentialKey))
			require.False(t, attached)
		})
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("private-upstream-detail"))
	}))
	defer upstream.Close()
	client, err := authenticationv1.NewForConfig(&rest.Config{Host: upstream.URL})
	require.NoError(t, err)
	mgr, err := auth.New(&config.Config{
		Auth:          config.AuthConfig{Mode: config.AuthModePassthrough},
		Authorization: config.AuthorizationConfig{Mode: config.AuthorizationModeBackendPolicy},
		Clusters:      []config.ClusterConfig{{Name: "target"}},
	}, auth.Dependencies{TargetReviewers: map[string]auth.TokenReviewer{
		"target": {Client: client, Authority: upstream.URL},
	}})
	require.NoError(t, err)
	core, logs := observer.New(zap.DebugLevel)
	engine := gin.New()
	engine.Use(middleware.Logging(zap.New(core)), func(c *gin.Context) {
		if _, ok := middleware.Authenticate(c, mgr); ok {
			c.Next()
		}
	})
	engine.GET("/clusters/:cluster", func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/clusters/target", nil)
	req.Header.Set("Authorization", "Bearer private-token")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	apiErr := decodeError(t, w)
	assert.Equal(t, auth.ErrIdentityUnavailable.Error(), apiErr.Message)
	require.Equal(t, 1, logs.Len())
	assert.Contains(t, logs.All()[0].ContextMap()["error"], "token review failed: status=403 timeout=false")
	for _, secret := range []string{"private-token", "private-upstream-detail"} {
		assert.NotContains(t, fmt.Sprint(logs.All()), secret)
		assert.NotContains(t, w.Body.String(), secret)
	}
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestLoginHandler(t *testing.T) {
	const credentials = `{"username":"Alice","password":" private-password\r\n ","cluster":"cluster","token":"private-token"}`
	pair := auth.TokenPair{AccessToken: "access-token", ExpiresAt: time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		name    string
		mode    string
		body    string
		err     error
		called  bool
		status  int
		code    string
		message string
	}{
		{name: "LDAP password login", mode: config.AuthModeLDAP, body: credentials, called: true, status: http.StatusOK},
		{name: "OpenShift password login", mode: config.AuthModeOpenShift, body: credentials, called: true, status: http.StatusOK},
		{name: "passthrough token login", mode: config.AuthModePassthrough, body: credentials, called: true, status: http.StatusOK},
		{name: "malformed LDAP JSON", mode: config.AuthModeLDAP, body: `{`, status: http.StatusBadRequest, code: apierrors.CodeBadRequest, message: "unexpected EOF"},
		{name: "missing LDAP username", mode: config.AuthModeLDAP, body: `{"password":"private-password"}`, status: http.StatusBadRequest, code: apierrors.CodeBadRequest, message: "username and password are required"},
		{name: "empty LDAP password", mode: config.AuthModeLDAP, body: `{"username":"Alice","password":""}`, status: http.StatusBadRequest, code: apierrors.CodeBadRequest, message: "username and password are required"},
		{name: "LDAP raw token rejected", mode: config.AuthModeLDAP, body: `{"cluster":"cluster","token":"private-token"}`, status: http.StatusBadRequest, code: apierrors.CodeBadRequest, message: "username and password are required"},
		{name: "LDAP bad credentials", mode: config.AuthModeLDAP, body: credentials, called: true, err: auth.ErrBadCredentials, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: "invalid credentials"},
		{name: "LDAP unauthenticated", mode: config.AuthModeLDAP, body: credentials, called: true, err: auth.ErrUnauthenticated, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: "invalid credentials"},
		{name: "LDAP directory unavailable", mode: config.AuthModeLDAP, body: credentials, called: true, err: auth.ErrDirectoryUnavailable, status: http.StatusServiceUnavailable, code: apierrors.CodeInternal, message: "directory service unavailable"},
		{name: "LDAP issuance failure", mode: config.AuthModeLDAP, body: credentials, called: true, err: errors.New("private-signing-key"), status: http.StatusInternalServerError, code: apierrors.CodeInternal, message: "internal server error"},
		{name: "OpenShift directory error unchanged", mode: config.AuthModeOpenShift, body: credentials, called: true, err: auth.ErrDirectoryUnavailable, status: http.StatusInternalServerError, code: apierrors.CodeInternal, message: "internal server error"},
		{name: "OpenShift unsupported login", mode: config.AuthModeOpenShift, body: credentials, called: true, err: auth.ErrNotSupported, status: http.StatusNotImplemented, code: apierrors.CodeNotSupported, message: `operation "Login" is not supported in the current configuration`},
		{name: "passthrough missing token", mode: config.AuthModePassthrough, body: `{"cluster":"cluster"}`, status: http.StatusBadRequest, code: apierrors.CodeBadRequest, message: "cluster and token are required"},
		{name: "passthrough unsupported login", mode: config.AuthModePassthrough, body: credentials, called: true, err: auth.ErrNotSupported, status: http.StatusNotImplemented, code: apierrors.CodeNotSupported, message: `operation "Login" is not supported in the current configuration`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			var loginErr error
			if tc.err != nil {
				loginErr = fmt.Errorf("private diagnostic: %w", tc.err)
			}
			mgr := &testutil.MockAuthManager{
				PasswordLoginFn: func(_ context.Context, username, password string) (auth.TokenPair, error) {
					called = true
					if tc.mode == config.AuthModePassthrough || username != "Alice" || password != " private-password\r\n " {
						t.Fatal("loginHandler() dispatched incorrect password credentials or mode")
					}
					return pair, loginErr
				},
				LoginFn: func(_ context.Context, cluster, token string) (auth.TokenPair, error) {
					called = true
					if tc.mode != config.AuthModePassthrough || cluster != "cluster" || token != "private-token" {
						t.Fatal("loginHandler() dispatched incorrect token credentials or mode")
					}
					return pair, loginErr
				},
			}
			core, logs := observer.New(zap.DebugLevel)
			router := gin.New()
			router.Use(middleware.Logging(zap.New(core)))
			downstream := false
			router.POST("/login", loginHandler(mgr, tc.mode), func(*gin.Context) { downstream = true })
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(tc.body)))
			if called != tc.called {
				t.Fatalf("loginHandler() called manager = %t, want %t", called, tc.called)
			}
			if logs.Len() != 1 {
				t.Fatalf("loginHandler() logged %d requests, want 1", logs.Len())
			}
			for _, secret := range []string{"private-password", "private-token", "private-signing-key", "private diagnostic", pair.AccessToken} {
				if strings.Contains(fmt.Sprint(logs.All()), secret) {
					t.Errorf("loginHandler() logged sensitive value %q", secret)
				}
			}
			if tc.status != http.StatusOK {
				checkAuthError(t, w, apierrors.APIError{Status: tc.status, Code: tc.code, Message: tc.message})
				if downstream {
					t.Fatal("loginHandler() ran downstream handler after failure")
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("loginHandler() status = %d, want 200", w.Code)
			}
			var got auth.TokenPair
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(pair, got); diff != "" {
				t.Errorf("loginHandler() token mismatch (-want +got):\n%s", diff)
			}
			wantCache := ""
			if tc.mode == config.AuthModeLDAP {
				wantCache = "no-store"
			}
			if got := w.Header().Get("Cache-Control"); got != wantCache {
				t.Errorf("loginHandler() Cache-Control = %q, want %q", got, wantCache)
			}
		})
	}
}

func TestRefreshHandler(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		body    io.Reader
		err     error
		called  bool
		status  int
		code    string
		message string
	}{
		{name: "LDAP without body", mode: config.AuthModeLDAP, body: http.NoBody, status: http.StatusNotImplemented, code: apierrors.CodeNotSupported, message: `operation "Refresh" is not supported in the current configuration`},
		{name: "LDAP body is not read", mode: config.AuthModeLDAP, body: iotest.ErrReader(errors.New("body must not be read")), status: http.StatusNotImplemented, code: apierrors.CodeNotSupported, message: `operation "Refresh" is not supported in the current configuration`},
		{name: "OpenShift refresh", mode: config.AuthModeOpenShift, called: true, status: http.StatusOK},
		{name: "OpenShift missing body", mode: config.AuthModeOpenShift, body: http.NoBody, status: http.StatusBadRequest, code: apierrors.CodeBadRequest, message: "EOF"},
		{name: "OpenShift expired refresh", mode: config.AuthModeOpenShift, called: true, err: auth.ErrTokenExpired, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: "refresh token is invalid or expired"},
		{name: "OpenShift rejected credentials", mode: config.AuthModeOpenShift, called: true, err: auth.ErrBadCredentials, status: http.StatusUnauthorized, code: apierrors.CodeUnauthorized, message: "refresh token rejected by identity provider"},
		{name: "OpenShift internal failure", mode: config.AuthModeOpenShift, called: true, err: errors.New("internal diagnostic"), status: http.StatusInternalServerError, code: apierrors.CodeInternal, message: "internal server error"},
		{name: "passthrough unsupported refresh", mode: config.AuthModePassthrough, called: true, err: auth.ErrNotSupported, status: http.StatusNotImplemented, code: apierrors.CodeNotSupported, message: `operation "Refresh" is not supported in the current configuration`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			pair := auth.TokenPair{AccessToken: "access", RefreshToken: "refresh"}
			mgr := &testutil.MockAuthManager{RefreshFn: func(_ context.Context, token string) (auth.TokenPair, error) {
				called = true
				if token != "refresh" {
					t.Fatalf("Refresh() token = %q, want refresh", token)
				}
				return pair, tc.err
			}}
			router := gin.New()
			downstream := false
			router.POST("/refresh", refreshHandler(mgr, tc.mode), func(*gin.Context) { downstream = true })
			body := tc.body
			if body == nil {
				body = strings.NewReader(`{"refreshToken":"refresh"}`)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/refresh", body))
			if called != tc.called {
				t.Fatalf("refreshHandler() called manager = %t, want %t", called, tc.called)
			}
			if tc.status != http.StatusOK {
				checkAuthError(t, w, apierrors.APIError{Status: tc.status, Code: tc.code, Message: tc.message})
				if downstream {
					t.Fatal("refreshHandler() ran downstream handler after failure")
				}
				return
			}
			var got auth.TokenPair
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || !cmp.Equal(pair, got) {
				t.Errorf("refreshHandler() = %d, %+v; want 200, %+v", w.Code, got, pair)
			}
		})
	}
}

func TestRegisterAuthRoutes(t *testing.T) {
	for _, mode := range []string{config.AuthModeLDAP, config.AuthModeOpenShift} {
		t.Run(mode, func(t *testing.T) {
			router := gin.New()
			registerAuthRoutes(router.Group("/api/v1/auth"), &testutil.MockAuthManager{}, mode)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/mode", nil))
			var got authModeResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || got.Mode != mode {
				t.Errorf("auth mode = %d, %q; want 200, %q", w.Code, got.Mode, mode)
			}
			want := map[string]string{
				"/api/v1/auth/mode": http.MethodGet, "/api/v1/auth/login": http.MethodPost, "/api/v1/auth/refresh": http.MethodPost,
			}
			if mode == config.AuthModeOpenShift {
				want["/api/v1/auth/openshift/authorize"] = http.MethodGet
				want["/api/v1/auth/openshift/callback"] = http.MethodPost
			}
			routes := make(map[string]string)
			for _, route := range router.Routes() {
				routes[route.Path] = route.Method
			}
			if diff := cmp.Diff(want, routes); diff != "" {
				t.Errorf("auth routes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNew(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			cfg := &config.Config{Auth: config.AuthConfig{Mode: config.AuthModeLDAP}}
			cfg.Auth.RateLimit.Enabled = enabled
			cfg.Auth.RateLimit.RequestsPerSecond = 0.001
			cfg.Auth.RateLimit.Burst = 1
			cfg.Auth.RateLimit.MaxIPTracked = 10
			calls := 0
			mgr := &testutil.MockAuthManager{PasswordLoginFn: func(context.Context, string, string) (auth.TokenPair, error) {
				calls++
				return auth.TokenPair{}, auth.ErrBadCredentials
			}}
			s := New(cfg, mgr, &testutil.MockClusterManager{}, &resources.Registry{}, zap.NewNop())
			for i := range 2 {
				w := httptest.NewRecorder()
				s.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"Alice","password":"secret"}`)))
				want := http.StatusUnauthorized
				if enabled && i == 1 {
					want = http.StatusTooManyRequests
				}
				if w.Code != want {
					t.Fatalf("login request %d status = %d, want %d", i+1, w.Code, want)
				}
			}
			wantCalls := 2
			if enabled {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Errorf("PasswordLogin() calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func checkAuthError(t *testing.T, w *httptest.ResponseRecorder, want apierrors.APIError) {
	t.Helper()
	if w.Code != want.Status {
		t.Fatalf("response status = %d, want %d: %s", w.Code, want.Status, w.Body)
	}
	var envelope struct {
		Error apierrors.APIError `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	got := envelope.Error
	if got.Code != want.Code || got.Message != want.Message || got.Status != want.Status || len(got.Details) != 0 {
		t.Errorf("response error = %+v, want %+v", got, want)
	}
}

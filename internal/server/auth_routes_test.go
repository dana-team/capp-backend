package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func authEngine(t *testing.T, mgr auth.AuthManager, mode string) *gin.Engine {
	t.Helper()
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	rg := engine.Group("/api/v1/auth")
	registerAuthRoutes(rg, mgr, mode)
	return engine
}

func postJSON(t *testing.T, engine *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	return w
}

// -- Mode endpoint --

func TestAuthMode_Returns200(t *testing.T) {
	engine := authEngine(t, &testutil.MockAuthManager{}, "jwt")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/mode", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "jwt", body["mode"])
}

// -- Login (non-dex mode) tests --

func TestLogin_NonDex_Success(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		LoginFn: func(_ context.Context, _ string, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{AccessToken: "at", RefreshToken: "rt"}, nil
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Cluster: "prod", Token: "k8s-tok"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLogin_NonDex_BadJSON(t *testing.T) {
	engine := authEngine(t, &testutil.MockAuthManager{}, "jwt")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLogin_NonDex_EmptyCluster(t *testing.T) {
	engine := authEngine(t, &testutil.MockAuthManager{}, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Cluster: "", Token: "tok"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLogin_NonDex_EmptyToken(t *testing.T) {
	engine := authEngine(t, &testutil.MockAuthManager{}, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Cluster: "prod", Token: ""})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLogin_NonDex_ErrNotSupported(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		LoginFn: func(_ context.Context, _ string, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, auth.ErrNotSupported
		},
	}
	engine := authEngine(t, mgr, "passthrough")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Cluster: "prod", Token: "tok"})
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestLogin_NonDex_ErrUnauthenticated(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		LoginFn: func(_ context.Context, _ string, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, auth.ErrUnauthenticated
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Cluster: "prod", Token: "bad-tok"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLogin_NonDex_OtherError(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		LoginFn: func(_ context.Context, _ string, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, errors.New("connection refused")
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Cluster: "prod", Token: "tok"})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// -- Login (dex mode) tests --

func TestLogin_Dex_Success(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		PasswordLoginFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{AccessToken: "at"}, nil
		},
	}
	engine := authEngine(t, mgr, "dex")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Username: "admin", Password: "pass"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLogin_Dex_EmptyUserPass(t *testing.T) {
	engine := authEngine(t, &testutil.MockAuthManager{}, "dex")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Username: "", Password: ""})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLogin_Dex_ErrBadCredentials(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		PasswordLoginFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, auth.ErrBadCredentials
		},
	}
	engine := authEngine(t, mgr, "dex")
	w := postJSON(t, engine, "/api/v1/auth/login", loginRequest{Username: "admin", Password: "wrong"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// -- Refresh tests --

func TestRefresh_Success(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		RefreshFn: func(_ context.Context, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{AccessToken: "new-at"}, nil
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/refresh", refreshRequest{RefreshToken: "rt"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRefresh_BadJSON(t *testing.T) {
	engine := authEngine(t, &testutil.MockAuthManager{}, "jwt")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRefresh_ErrNotSupported(t *testing.T) {
	engine := authEngine(t, &testutil.MockAuthManager{}, "passthrough")
	w := postJSON(t, engine, "/api/v1/auth/refresh", refreshRequest{RefreshToken: "rt"})
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestRefresh_ErrTokenExpired(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		RefreshFn: func(_ context.Context, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, auth.ErrTokenExpired
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/refresh", refreshRequest{RefreshToken: "expired"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefresh_ErrInvalidToken(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		RefreshFn: func(_ context.Context, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, auth.ErrInvalidToken
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/refresh", refreshRequest{RefreshToken: "invalid"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefresh_ErrBadCredentials(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		RefreshFn: func(_ context.Context, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, auth.ErrBadCredentials
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/refresh", refreshRequest{RefreshToken: "rt"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefresh_OtherError(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		RefreshFn: func(_ context.Context, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, errors.New("db failure")
		},
	}
	engine := authEngine(t, mgr, "jwt")
	w := postJSON(t, engine, "/api/v1/auth/refresh", refreshRequest{RefreshToken: "rt"})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// -- OpenShift authorize tests --

func TestOpenshiftAuthorize_Success(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		GetAuthorizeURLFn: func() (string, error) {
			return "https://oauth.example.com/authorize?client_id=capp", nil
		},
	}
	engine := authEngine(t, mgr, "openshift")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/openshift/authorize", nil))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body["authorizeUrl"], "oauth.example.com")
}

func TestOpenshiftAuthorize_Error(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		GetAuthorizeURLFn: func() (string, error) {
			return "", errors.New("config error")
		},
	}
	engine := authEngine(t, mgr, "openshift")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/openshift/authorize", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestOpenshiftAuthorize_WrongManagerType(t *testing.T) {
	mgr := &noOAuthAuthManager{}
	engine := authEngine(t, mgr, "openshift")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/openshift/authorize", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// -- OpenShift callback tests --

func TestOpenshiftCallback_Success(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		OAuthExchangeFn: func(_ context.Context, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{AccessToken: "os-at"}, nil
		},
	}
	engine := authEngine(t, mgr, "openshift")
	w := postJSON(t, engine, "/api/v1/auth/openshift/callback", openshiftCallbackRequest{Code: "authz-code"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestOpenshiftCallback_BadJSON(t *testing.T) {
	mgr := &testutil.MockAuthManager{}
	engine := authEngine(t, mgr, "openshift")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/openshift/callback", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestOpenshiftCallback_ErrBadCredentials(t *testing.T) {
	mgr := &testutil.MockAuthManager{
		OAuthExchangeFn: func(_ context.Context, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, auth.ErrBadCredentials
		},
	}
	engine := authEngine(t, mgr, "openshift")
	w := postJSON(t, engine, "/api/v1/auth/openshift/callback", openshiftCallbackRequest{Code: "bad"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// noOAuthAuthManager implements AuthManager but NOT OAuthAuthorizer.
type noOAuthAuthManager struct{}

func (m *noOAuthAuthManager) Authenticate(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
	return auth.ClusterCredential{}, nil
}
func (m *noOAuthAuthManager) Login(context.Context, string, string) (auth.TokenPair, error) {
	return auth.TokenPair{}, auth.ErrNotSupported
}
func (m *noOAuthAuthManager) PasswordLogin(context.Context, string, string) (auth.TokenPair, error) {
	return auth.TokenPair{}, auth.ErrNotSupported
}
func (m *noOAuthAuthManager) Refresh(context.Context, string) (auth.TokenPair, error) {
	return auth.TokenPair{}, auth.ErrNotSupported
}

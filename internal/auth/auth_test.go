package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Passthrough tests ─────────────────────────────────────────────────────────

func TestPassthrough_Authenticate_MissingHeader(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.True(t, errors.Is(err, ErrUnauthenticated))
}

func TestPassthrough_Authenticate_MalformedHeader(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.True(t, errors.Is(err, ErrUnauthenticated))
}

func TestPassthrough_Authenticate_EmptyToken(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer ")
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.True(t, errors.Is(err, ErrUnauthenticated))
}

func TestPassthrough_Authenticate_Valid(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer my-k8s-token")
	cred, err := m.Authenticate(context.Background(), "prod", r)
	require.NoError(t, err)
	assert.Equal(t, "my-k8s-token", cred.BearerToken)
}

func TestPassthrough_Login_NotSupported(t *testing.T) {
	m := newPassthroughManager()
	_, err := m.Login(context.Background(), "prod", "tok")
	assert.True(t, errors.Is(err, ErrNotSupported))
}

func TestPassthrough_Refresh_NotSupported(t *testing.T) {
	m := newPassthroughManager()
	_, err := m.Refresh(context.Background(), "tok")
	assert.True(t, errors.Is(err, ErrNotSupported))
}

// ── Static tests ──────────────────────────────────────────────────────────────

func TestStatic_Authenticate_UnknownKey(t *testing.T) {
	m := newStaticManager([]string{"valid-key"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer wrong-key")
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.True(t, errors.Is(err, ErrUnauthenticated))
}

func TestStatic_Authenticate_KnownKey(t *testing.T) {
	m := newStaticManager([]string{"dev-key-1", "dev-key-2"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer dev-key-2")
	cred, err := m.Authenticate(context.Background(), "prod", r)
	require.NoError(t, err)
	// Static mode returns an empty BearerToken — see staticManager godoc.
	assert.Empty(t, cred.BearerToken)
}

func TestStatic_Authenticate_MissingHeader(t *testing.T) {
	m := newStaticManager([]string{"k"})
	_, err := m.Authenticate(context.Background(), "prod", httptest.NewRequest(http.MethodGet, "/", nil))
	assert.True(t, errors.Is(err, ErrUnauthenticated))
}

func TestStatic_Login_NotSupported(t *testing.T) {
	m := newStaticManager([]string{"k"})
	_, err := m.Login(context.Background(), "prod", "k")
	assert.True(t, errors.Is(err, ErrNotSupported))
}

// ── JWT tests ─────────────────────────────────────────────────────────────────

// mockClusterServer returns a test HTTP server that accepts GET /version with
// the given token and returns 200 (or 401 for any other token).
func mockClusterServer(t *testing.T, validToken string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			tok := r.Header.Get("Authorization")
			if tok == "Bearer "+validToken {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
}

func makeJWTManager(t *testing.T, clusterURL string) *jwtManager {
	t.Helper()
	cfg := &config.JWTConfig{
		SecretKey:         "test-secret-key-32-bytes-long!!",
		TokenTTLMinutes:   60,
		RefreshTTLMinutes: 1440,
	}
	return newJWTManager(cfg, map[string]string{"prod": clusterURL})
}

func TestJWT_Login_ValidToken(t *testing.T) {
	srv := mockClusterServer(t, "good-token")
	defer srv.Close()

	m := makeJWTManager(t, srv.URL)
	pair, err := m.Login(context.Background(), "prod", "good-token")
	require.NoError(t, err)
	assert.NotEmpty(t, pair.AccessToken)
	assert.NotEmpty(t, pair.RefreshToken)
	assert.True(t, pair.ExpiresAt.After(time.Now()))
}

func TestJWT_Login_InvalidToken(t *testing.T) {
	srv := mockClusterServer(t, "good-token")
	defer srv.Close()

	m := makeJWTManager(t, srv.URL)
	_, err := m.Login(context.Background(), "prod", "bad-token")
	assert.True(t, errors.Is(err, ErrUnauthenticated))
}

func TestJWT_Authenticate_ValidAccessToken(t *testing.T) {
	srv := mockClusterServer(t, "my-k8s-token")
	defer srv.Close()

	m := makeJWTManager(t, srv.URL)
	pair, err := m.Login(context.Background(), "prod", "my-k8s-token")
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	cred, err := m.Authenticate(context.Background(), "prod", r)
	require.NoError(t, err)
	assert.Equal(t, "my-k8s-token", cred.BearerToken)
}

func TestJWT_Authenticate_RefreshTokenRejected(t *testing.T) {
	srv := mockClusterServer(t, "tok")
	defer srv.Close()

	m := makeJWTManager(t, srv.URL)
	pair, err := m.Login(context.Background(), "prod", "tok")
	require.NoError(t, err)

	// Providing the refresh token where an access token is expected should fail.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+pair.RefreshToken)
	_, err = m.Authenticate(context.Background(), "prod", r)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidToken))
}

func TestJWT_Refresh_ValidCycle(t *testing.T) {
	srv := mockClusterServer(t, "tok")
	defer srv.Close()

	m := makeJWTManager(t, srv.URL)
	pair1, err := m.Login(context.Background(), "prod", "tok")
	require.NoError(t, err)

	pair2, err := m.Refresh(context.Background(), pair1.RefreshToken)
	require.NoError(t, err)
	// New pair should have different tokens (new JWTs with new IssuedAt).
	assert.NotEmpty(t, pair2.AccessToken)
}

func TestJWT_Refresh_AccessTokenRejected(t *testing.T) {
	srv := mockClusterServer(t, "tok")
	defer srv.Close()

	m := makeJWTManager(t, srv.URL)
	pair, err := m.Login(context.Background(), "prod", "tok")
	require.NoError(t, err)

	// Using the access token where a refresh token is expected should fail.
	_, err = m.Refresh(context.Background(), pair.AccessToken)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidToken))
}

func TestJWT_Authenticate_MissingHeader(t *testing.T) {
	m := makeJWTManager(t, "http://unused")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.True(t, errors.Is(err, ErrUnauthenticated))
}

func TestJWT_Authenticate_InvalidSignature(t *testing.T) {
	m := makeJWTManager(t, "http://unused")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not.a.jwt")
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.True(t, errors.Is(err, ErrInvalidToken))
}

// ── Factory tests ─────────────────────────────────────────────────────────────

func TestNew_Passthrough(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{Mode: "passthrough"}}
	m, err := New(cfg)
	require.NoError(t, err)
	assert.IsType(t, &passthroughManager{}, m)
}

func TestNew_Static(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{
		Mode:   "static",
		Static: config.StaticConfig{APIKeys: []string{"k"}},
	}}
	m, err := New(cfg)
	require.NoError(t, err)
	assert.IsType(t, &staticManager{}, m)
}

func TestNew_JWT(t *testing.T) {
	cfg := &config.Config{
		Auth: config.AuthConfig{
			Mode: "jwt",
			JWT: config.JWTConfig{
				SecretKey:         "test-secret",
				TokenTTLMinutes:   60,
				RefreshTTLMinutes: 1440,
			},
		},
	}
	m, err := New(cfg)
	require.NoError(t, err)
	assert.IsType(t, &jwtManager{}, m)
}

func TestNew_UnknownMode(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{Mode: "magic"}}
	_, err := New(cfg)
	require.Error(t, err)
}

// ── Dex tests ─────────────────────────────────────────────────────────────────

// mockDexServer returns an httptest.Server that serves:
//
//	/.well-known/openid-configuration  — OIDC discovery pointing to itself
//	/keys                              — empty JWKS (no real tokens needed for factory tests)
//	/token                             — configurable token response
func mockDexServer(t *testing.T, tokenStatus int, tokenBody string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
                "issuer": %q,
                "authorization_endpoint": %q,
                "token_endpoint": %q,
                "jwks_uri": %q
            }`, srv.URL, srv.URL+"/auth", srv.URL+"/token", srv.URL+"/keys")
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"keys":[]}`)
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tokenStatus)
			fmt.Fprint(w, tokenBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNew_Dex(t *testing.T) {
	srv := mockDexServer(t, http.StatusOK, `{}`)
	cfg := &config.Config{
		Auth: config.AuthConfig{
			Mode: "dex",
			Dex: config.DexConfig{
				Endpoint:     srv.URL,
				ClientID:     "capp",
				ClientSecret: "secret",
				Scopes:       []string{"openid"},
			},
			JWT: config.JWTConfig{SecretKey: "test-key"},
		},
	}
	mgr, err := New(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, mgr)
}

func TestDex_PasswordLogin_BadCredentials(t *testing.T) {
	srv := mockDexServer(t, http.StatusUnauthorized, `{"error":"invalid_grant","error_description":"Invalid credentials"}`)
	cfg := &config.Config{
		Auth: config.AuthConfig{
			Mode: "dex",
			Dex: config.DexConfig{
				Endpoint:     srv.URL,
				ClientID:     "capp",
				ClientSecret: "secret",
				Scopes:       []string{"openid"},
			},
			JWT: config.JWTConfig{SecretKey: "test-key"},
		},
	}
	mgr, err := New(cfg)
	require.NoError(t, err)

	_, err = mgr.PasswordLogin(context.Background(), "user", "wrongpassword")
	assert.ErrorIs(t, err, ErrBadCredentials)
}

func TestDex_Login_NotSupported(t *testing.T) {
	srv := mockDexServer(t, http.StatusOK, `{}`)
	cfg := &config.Config{
		Auth: config.AuthConfig{
			Mode: "dex",
			Dex: config.DexConfig{
				Endpoint:     srv.URL,
				ClientID:     "capp",
				ClientSecret: "secret",
				Scopes:       []string{"openid"},
			},
			JWT: config.JWTConfig{SecretKey: "test-key"},
		},
	}
	mgr, err := New(cfg)
	require.NoError(t, err)

	_, err = mgr.Login(context.Background(), "cluster", "token")
	assert.ErrorIs(t, err, ErrNotSupported)
}

func TestDex_PasswordLogin_NotSupported_On_JWT(t *testing.T) {
	// jwtManager should return ErrNotSupported for PasswordLogin
	cfg := &config.Config{
		Auth: config.AuthConfig{
			Mode: "jwt",
			JWT:  config.JWTConfig{SecretKey: "test-key"},
		},
	}
	mgr, err := New(cfg)
	require.NoError(t, err)

	_, err = mgr.PasswordLogin(context.Background(), "user", "pass")
	assert.ErrorIs(t, err, ErrNotSupported)
}

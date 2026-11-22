package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	oAuthTokenURLSuffix     = "/oauth/token"
	oAuthAuthorizeURLSuffix = "/oauth/authorize"
)

func newTestOpenShiftManager(t *testing.T, oauthServer *httptest.Server) *openShiftManager {
	t.Helper()

	osCfg := &config.OpenShiftConfig{
		APIServer:            oauthServer.URL,
		ClientID:             "test-client",
		ClientSecret:         "test-secret",
		RedirectURI:          "https://app.example.com/callback",
		Scopes:               []string{"user:info"},
		TokenCacheTTLSeconds: 60,
	}

	return &openShiftManager{
		cfg:        osCfg,
		httpClient: oauthServer.Client(),
		oauthMeta: &oauthServerMeta{
			AuthorizationEndpoint: oauthServer.URL + oAuthAuthorizeURLSuffix,
			TokenEndpoint:         oauthServer.URL + oAuthTokenURLSuffix,
		},
		cacheTTL: 60 * time.Second,
		hmacKey:  deriveStateKey(osCfg.ClientSecret),
	}
}

func TestOpenShiftManagerOAuthExchangeValidCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == oAuthTokenURLSuffix {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oauthTokenResponse{
				AccessToken:  "access-tok",
				RefreshToken: "refresh-tok",
				ExpiresIn:    3600,
				TokenType:    "Bearer",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	pair, err := mgr.OAuthExchange(context.Background(), "auth-code", "")
	require.NoError(t, err)
	assert.Equal(t, "access-tok", pair.AccessToken)
	assert.Equal(t, "refresh-tok", pair.RefreshToken)
	assert.False(t, pair.ExpiresAt.IsZero())
}

func TestOpenShiftManagerOAuthExchangeInvalidCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, err := mgr.OAuthExchange(context.Background(), "bad-code", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadCredentials)
}

func TestOpenShiftManagerRefreshValidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == oAuthTokenURLSuffix {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oauthTokenResponse{
				AccessToken:  "new-access",
				RefreshToken: "new-refresh",
				ExpiresIn:    3600,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	pair, err := mgr.Refresh(context.Background(), "old-refresh-token")
	require.NoError(t, err)
	assert.Equal(t, "new-access", pair.AccessToken)
	assert.Equal(t, "new-refresh", pair.RefreshToken)
}

func TestOpenShiftManagerLoginNotSupported(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, err := mgr.Login(context.Background(), "cluster", "token")
	assert.ErrorIs(t, err, ErrNotSupported)
}

func TestOpenShiftManagerPasswordLoginSuccess(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oAuthAuthorizeURLSuffix {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="openshift"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "user", user)
		assert.Equal(t, "pass", pass)
		w.Header().Set("Location", srv.URL+"/oauth/token/implicit#access_token=access-tok&expires_in=3600&token_type=bearer")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	pair, err := mgr.PasswordLogin(context.Background(), "user", "pass")
	require.NoError(t, err)
	assert.Equal(t, "access-tok", pair.AccessToken)
	assert.Empty(t, pair.RefreshToken) // implicit grant: no refresh token
	assert.False(t, pair.ExpiresAt.IsZero())
}

func TestOpenShiftManagerPasswordLoginBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oAuthAuthorizeURLSuffix {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="openshift"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, err := mgr.PasswordLogin(context.Background(), "user", "wrongpass")
	assert.ErrorIs(t, err, ErrBadCredentials)
}

func TestOpenShiftManagerPasswordLoginQueryParamToken(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oAuthAuthorizeURLSuffix {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="openshift"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Some OpenShift versions put the token in query params rather than fragment.
		w.Header().Set("Location", srv.URL+"/oauth/token/implicit?access_token=qp-tok&expires_in=1800&token_type=bearer")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	pair, err := mgr.PasswordLogin(context.Background(), "user", "pass")
	require.NoError(t, err)
	assert.Equal(t, "qp-tok", pair.AccessToken)
	assert.False(t, pair.ExpiresAt.IsZero())
}

func TestOpenShiftManagerPasswordLoginMissingExpiresIn(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oAuthAuthorizeURLSuffix {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="openshift"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// No expires_in — ExpiresAt should be zero (unknown), not time.Now().
		w.Header().Set("Location", srv.URL+"/oauth/token/implicit#access_token=notok&token_type=bearer")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	pair, err := mgr.PasswordLogin(context.Background(), "user", "pass")
	require.NoError(t, err)
	assert.Equal(t, "notok", pair.AccessToken)
	assert.True(t, pair.ExpiresAt.IsZero()) // unknown expiry, not immediately expired
}

func TestOpenShiftManagerPasswordLoginOAuthErrorInRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oAuthAuthorizeURLSuffix {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="openshift"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Redirect with an access_denied credential error.
		w.Header().Set("Location", srv.URL+"/oauth/token/implicit#error=access_denied&error_description=bad+creds")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, err := mgr.PasswordLogin(context.Background(), "user", "pass")
	assert.ErrorIs(t, err, ErrBadCredentials)
}

func TestOpenShiftManagerPasswordLoginServerErrorInRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oAuthAuthorizeURLSuffix {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="openshift"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Non-credential server error must NOT map to ErrBadCredentials.
		w.Header().Set("Location", srv.URL+"/oauth/token/implicit#error=server_error&error_description=internal+error")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, err := mgr.PasswordLogin(context.Background(), "user", "pass")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrBadCredentials)
}

func TestOpenShiftManagerGetAuthorizeURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	authURL, state, err := mgr.GetAuthorizeURL("")
	require.NoError(t, err)
	assert.Contains(t, authURL, oAuthAuthorizeURLSuffix)
	assert.Contains(t, authURL, "client_id=test-client")
	assert.Contains(t, authURL, "response_type=code")
	assert.Contains(t, authURL, "redirect_uri=")
	assert.Contains(t, authURL, "state="+url.QueryEscape(state))
	// Stateless signed token: "<expUnix>:<nonce>.<sig>".
	assert.Contains(t, state, ".")
}

func TestOpenShiftManagerGetAuthorizeURLCustomRedirectURI(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	authURL, state, err := mgr.GetAuthorizeURL("http://localhost:18085/callback")
	require.NoError(t, err)
	assert.Contains(t, authURL, "redirect_uri="+url.QueryEscape("http://localhost:18085/callback"))
	assert.NotEmpty(t, state)
}

func TestOpenShiftManagerGetAuthorizeURLUniqueStates(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, state1, err := mgr.GetAuthorizeURL("")
	require.NoError(t, err)
	_, state2, err := mgr.GetAuthorizeURL("")
	require.NoError(t, err)
	assert.NotEqual(t, state1, state2)
}

func TestOpenShiftManagerValidateStateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, state, err := mgr.GetAuthorizeURL("")
	require.NoError(t, err)

	err = mgr.ValidateState(state)
	assert.NoError(t, err)
}

// Stateless tokens cannot be marked single-use without shared storage, so a
// valid, unexpired state validates repeatedly. This is safe because the OAuth
// authorization code is single-use at the token endpoint.
func TestOpenShiftManagerValidateStateStatelessReplay(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, state, err := mgr.GetAuthorizeURL("")
	require.NoError(t, err)

	require.NoError(t, mgr.ValidateState(state))
	require.NoError(t, mgr.ValidateState(state))
}

// A tampered payload must fail signature verification.
func TestOpenShiftManagerValidateStateTamperedSignature(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	_, state, err := mgr.GetAuthorizeURL("")
	require.NoError(t, err)

	err = mgr.ValidateState(state + "tampered")
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

// A state signed with a different client secret (i.e. not by this backend)
// must be rejected.
func TestOpenShiftManagerValidateStateWrongKey(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	other := newTestOpenShiftManager(t, srv)
	other.cfg.ClientSecret = "different-secret"
	other.hmacKey = deriveStateKey(other.cfg.ClientSecret)

	_, state, err := other.GetAuthorizeURL("")
	require.NoError(t, err)

	err = mgr.ValidateState(state)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestOpenShiftManagerValidateStateUnknownState(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)
	err := mgr.ValidateState("nonexistent-state")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestOpenShiftManagerValidateStateExpired(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv)

	// Craft a correctly-signed but already-expired state token.
	payload := strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10) + ":deadbeef"
	state := payload + "." + mgr.stateSignature(payload)

	err := mgr.ValidateState(state)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

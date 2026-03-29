package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// tokenReviewReactor returns a reactor that responds to TokenReview create
// calls with the given authentication result.
func tokenReviewReactor(authenticated bool, username string, groups []string) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		review.Status = authv1.TokenReviewStatus{
			Authenticated: authenticated,
			User: authv1.UserInfo{
				Username: username,
				Groups:   groups,
			},
		}
		return true, review, nil
	}
}

func newTestOpenShiftManager(t *testing.T, oauthServer *httptest.Server, reactor k8stesting.ReactionFunc) *openShiftManager {
	t.Helper()

	fakeClient := fake.NewClientset()
	if reactor != nil {
		fakeClient.PrependReactor("create", "tokenreviews", reactor)
	}

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
		k8sClient:  fakeClient,
		oauthMeta: &oauthServerMeta{
			AuthorizationEndpoint: oauthServer.URL + "/oauth/authorize",
			TokenEndpoint:         oauthServer.URL + "/oauth/token",
		},
		cacheTTL: 60 * time.Second,
	}
}

func TestOpenShift_Authenticate_ValidToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	reactor := tokenReviewReactor(true, "jane", []string{"developers", "system:authenticated"})
	mgr := newTestOpenShiftManager(t, srv, reactor)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer valid-token")

	cred, err := mgr.Authenticate(context.Background(), "any-cluster", req)
	require.NoError(t, err)
	assert.Equal(t, "jane", cred.ImpersonateUser)
	assert.Equal(t, []string{"developers", "system:authenticated"}, cred.ImpersonateGroups)
	assert.Empty(t, cred.BearerToken)
}

func TestOpenShift_Authenticate_InvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	reactor := tokenReviewReactor(false, "", nil)
	mgr := newTestOpenShiftManager(t, srv, reactor)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer bad-token")

	_, err := mgr.Authenticate(context.Background(), "any-cluster", req)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestOpenShift_Authenticate_NoHeader(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := mgr.Authenticate(context.Background(), "any-cluster", req)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestOpenShift_Authenticate_CacheHit(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	callCount := 0
	reactor := func(action k8stesting.Action) (bool, runtime.Object, error) {
		callCount++
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		review.Status = authv1.TokenReviewStatus{
			Authenticated: true,
			User:          authv1.UserInfo{Username: "jane", Groups: []string{"devs"}},
		}
		return true, review, nil
	}

	mgr := newTestOpenShiftManager(t, srv, reactor)

	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("Authorization", "Bearer my-token")
	cred1, err := mgr.Authenticate(context.Background(), "", req1)
	require.NoError(t, err)
	assert.Equal(t, "jane", cred1.ImpersonateUser)
	assert.Equal(t, 1, callCount)

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer my-token")
	cred2, err := mgr.Authenticate(context.Background(), "", req2)
	require.NoError(t, err)
	assert.Equal(t, "jane", cred2.ImpersonateUser)
	assert.Equal(t, 1, callCount) // Cache hit.
}

func TestOpenShift_Authenticate_CacheExpired(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	callCount := 0
	reactor := func(action k8stesting.Action) (bool, runtime.Object, error) {
		callCount++
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		review.Status = authv1.TokenReviewStatus{
			Authenticated: true,
			User:          authv1.UserInfo{Username: "jane", Groups: []string{"devs"}},
		}
		return true, review, nil
	}

	mgr := newTestOpenShiftManager(t, srv, reactor)
	mgr.cacheTTL = 1 * time.Millisecond

	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("Authorization", "Bearer my-token")
	_, err := mgr.Authenticate(context.Background(), "", req1)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	time.Sleep(5 * time.Millisecond)

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer my-token")
	_, err = mgr.Authenticate(context.Background(), "", req2)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount) // Cache expired.
}

func TestOpenShift_OAuthExchange_ValidCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
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

	mgr := newTestOpenShiftManager(t, srv, nil)
	pair, err := mgr.OAuthExchange(context.Background(), "auth-code")
	require.NoError(t, err)
	assert.Equal(t, "access-tok", pair.AccessToken)
	assert.Equal(t, "refresh-tok", pair.RefreshToken)
	assert.False(t, pair.ExpiresAt.IsZero())
}

func TestOpenShift_OAuthExchange_InvalidCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}))
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv, nil)
	_, err := mgr.OAuthExchange(context.Background(), "bad-code")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrBadCredentials)
}

func TestOpenShift_Refresh_ValidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
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

	mgr := newTestOpenShiftManager(t, srv, nil)
	pair, err := mgr.Refresh(context.Background(), "old-refresh-token")
	require.NoError(t, err)
	assert.Equal(t, "new-access", pair.AccessToken)
	assert.Equal(t, "new-refresh", pair.RefreshToken)
}

func TestOpenShift_Login_NotSupported(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv, nil)
	_, err := mgr.Login(context.Background(), "cluster", "token")
	assert.ErrorIs(t, err, ErrNotSupported)
}

func TestOpenShift_PasswordLogin_NotSupported(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv, nil)
	_, err := mgr.PasswordLogin(context.Background(), "user", "pass")
	assert.ErrorIs(t, err, ErrNotSupported)
}

func TestOpenShift_GetAuthorizeURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	mgr := newTestOpenShiftManager(t, srv, nil)
	authURL, err := mgr.GetAuthorizeURL()
	require.NoError(t, err)
	assert.Contains(t, authURL, "/oauth/authorize")
	assert.Contains(t, authURL, "client_id=test-client")
	assert.Contains(t, authURL, "response_type=code")
	assert.Contains(t, authURL, "redirect_uri=")
}

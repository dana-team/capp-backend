package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// openShiftManager implements AuthManager for auth.mode = "openshift".
//
// Dual-path authentication:
//  1. Browser path (OAuth Authorization Code): the frontend redirects the user
//     to the OpenShift OAuth server. After login, OpenShift redirects back with
//     an auth code. The frontend sends the code to POST /auth/openshift/callback
//     and the backend exchanges it for an OAuth access token.
//  2. Programmatic path: the client sends a raw K8s/OpenShift bearer token in
//     the Authorization header. No separate login endpoint needed.
//
// On every authenticated request the bearer token is validated via TokenReview
// against the home cluster (kubernetes.default.svc). The extracted user identity
// (username + groups) is returned in ClusterCredential so that the cluster
// middleware can apply K8s impersonation headers on all managed clusters.
//
// The backend is fully stateless: tokens are managed by OpenShift, not by the
// backend. A short-lived in-memory cache avoids a TokenReview on every request.
type openShiftManager struct {
	cfg        *config.OpenShiftConfig
	httpClient *http.Client

	// k8sClient is a typed Kubernetes client configured with rest.InClusterConfig().
	// Used to submit TokenReview requests against the home cluster.
	k8sClient kubernetes.Interface

	// oauthMeta holds the discovered OAuth server endpoints.
	oauthMeta *oauthServerMeta

	// tokenCache maps SHA-256 token hashes to cached validation results.
	tokenCache sync.Map

	// cacheTTL is how long a token validation result is cached.
	cacheTTL time.Duration
}

// oauthServerMeta holds the OAuth server endpoints discovered from the
// OpenShift API server's .well-known/oauth-authorization-server metadata.
type oauthServerMeta struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// cachedIdentity is an in-memory cache entry for a validated token.
type cachedIdentity struct {
	username  string
	groups    []string
	expiresAt time.Time
}

// newOpenShiftManager builds an openShiftManager. It creates an in-cluster
// K8s client for TokenReview and discovers OAuth endpoints from the configured
// API server.
func newOpenShiftManager(cfg *config.Config) (*openShiftManager, error) {
	osCfg := &cfg.Auth.OpenShift

	// Build the HTTP client for OAuth requests.
	httpClient := &http.Client{Timeout: 15 * time.Second}
	if osCfg.CACert != "" {
		transport, err := buildTLSTransport(osCfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("openshift: building TLS transport: %w", err)
		}
		httpClient.Transport = transport
	}

	// Build in-cluster K8s client for TokenReview.
	inClusterCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("openshift: in-cluster config required: %w", err)
	}
	k8sClient, err := kubernetes.NewForConfig(inClusterCfg)
	if err != nil {
		return nil, fmt.Errorf("openshift: building K8s client: %w", err)
	}

	// Discover OAuth endpoints.
	oauthMeta, err := discoverOAuthMeta(httpClient, osCfg.APIServer)
	if err != nil {
		// Fall back to standard OpenShift paths.
		oauthMeta = &oauthServerMeta{
			AuthorizationEndpoint: osCfg.APIServer + "/oauth/authorize",
			TokenEndpoint:         osCfg.APIServer + "/oauth/token",
		}
	}

	cacheTTL := time.Duration(osCfg.TokenCacheTTLSeconds) * time.Second
	if cacheTTL <= 0 {
		cacheTTL = 60 * time.Second
	}

	return &openShiftManager{
		cfg:        osCfg,
		httpClient: httpClient,
		k8sClient:  k8sClient,
		oauthMeta:  oauthMeta,
		cacheTTL:   cacheTTL,
	}, nil
}

// discoverOAuthMeta fetches the OpenShift OAuth server metadata from the
// well-known endpoint.
func discoverOAuthMeta(client *http.Client, apiServer string) (*oauthServerMeta, error) {
	discoveryURL := strings.TrimRight(apiServer, "/") + "/.well-known/oauth-authorization-server"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building discovery request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading discovery response: %w", err)
	}

	var meta oauthServerMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("parsing discovery response: %w", err)
	}

	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return nil, fmt.Errorf("discovery response missing required endpoints")
	}

	return &meta, nil
}

// ── AuthManager implementation ──────────────────────────────────────────────

// Authenticate validates the bearer token via TokenReview (with cache) and
// returns a ClusterCredential with impersonation fields set.
func (m *openShiftManager) Authenticate(_ context.Context, _ string, r *http.Request) (ClusterCredential, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ClusterCredential{}, ErrUnauthenticated
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return ClusterCredential{}, ErrUnauthenticated
	}
	token := strings.TrimPrefix(raw, prefix)

	// Check cache first.
	cacheKey := sha256Sum(token)
	if val, ok := m.tokenCache.Load(cacheKey); ok {
		cached := val.(*cachedIdentity)
		if time.Now().Before(cached.expiresAt) {
			return ClusterCredential{
				ImpersonateUser:   cached.username,
				ImpersonateGroups: cached.groups,
			}, nil
		}
		// Expired — remove and re-validate.
		m.tokenCache.Delete(cacheKey)
	}

	// Submit TokenReview against the home cluster.
	review := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	result, err := m.k8sClient.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return ClusterCredential{}, fmt.Errorf("openshift: token review failed: %w", err)
	}

	if !result.Status.Authenticated {
		return ClusterCredential{}, ErrUnauthenticated
	}

	username := result.Status.User.Username
	groups := result.Status.User.Groups

	// Cache the result.
	m.tokenCache.Store(cacheKey, &cachedIdentity{
		username:  username,
		groups:    groups,
		expiresAt: time.Now().Add(m.cacheTTL),
	})

	return ClusterCredential{
		ImpersonateUser:   username,
		ImpersonateGroups: groups,
	}, nil
}

// Login is not supported in openshift mode. Programmatic users send their
// token directly in the Authorization header.
func (m *openShiftManager) Login(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// PasswordLogin is not supported in openshift mode.
func (m *openShiftManager) PasswordLogin(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// Refresh exchanges an OpenShift refresh token for a new access token at the
// OAuth token endpoint.
func (m *openShiftManager) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {m.cfg.ClientID},
		"client_secret": {m.cfg.ClientSecret},
	}

	return m.oauthTokenRequest(ctx, form)
}

// ── OAuthAuthorizer implementation ──────────────────────────────────────────

// GetAuthorizeURL builds the OAuth authorize URL for the frontend to redirect
// the user's browser to.
func (m *openShiftManager) GetAuthorizeURL() (string, error) {
	params := url.Values{
		"client_id":     {m.cfg.ClientID},
		"redirect_uri":  {m.cfg.RedirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(m.cfg.Scopes, " ")},
	}
	return m.oauthMeta.AuthorizationEndpoint + "?" + params.Encode(), nil
}

// OAuthExchange exchanges an OAuth authorization code for tokens.
func (m *openShiftManager) OAuthExchange(ctx context.Context, code, redirectURI string) (TokenPair, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {m.cfg.ClientID},
		"client_secret": {m.cfg.ClientSecret},
	}

	return m.oauthTokenRequest(ctx, form)
}

// ── Cache cleanup ───────────────────────────────────────────────────────────

// StartCleanup runs a background goroutine that evicts expired cache entries.
// Call as: go mgr.StartCleanup(ctx)
func (m *openShiftManager) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			m.tokenCache.Range(func(key, value any) bool {
				if cached, ok := value.(*cachedIdentity); ok && now.After(cached.expiresAt) {
					m.tokenCache.Delete(key)
				}
				return true
			})
		}
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// oauthTokenResponse is the JSON structure returned by the OpenShift OAuth
// token endpoint.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// oauthTokenRequest POSTs to the OAuth token endpoint and returns a TokenPair.
func (m *openShiftManager) oauthTokenRequest(ctx context.Context, form url.Values) (TokenPair, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.oauthMeta.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return TokenPair{}, fmt.Errorf("openshift: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return TokenPair{}, fmt.Errorf("openshift: token request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenPair{}, fmt.Errorf("openshift: reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var oauthErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		return TokenPair{}, fmt.Errorf("%w: oauth server returned %q (status %d)",
			ErrBadCredentials, oauthErr.Error, resp.StatusCode)
	}

	var tokenResp oauthTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return TokenPair{}, fmt.Errorf("openshift: parsing token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return TokenPair{}, fmt.Errorf("openshift: token response missing access_token")
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return TokenPair{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

// sha256Sum returns a hex-encoded SHA-256 hash of the input string.
func sha256Sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

	// pendingStates maps CSRF state tokens to pendingState entries.
	// Entries are created by GetAuthorizeURL and consumed by ValidateState.
	pendingStates sync.Map

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

// pendingState is a short-lived CSRF state token issued by GetAuthorizeURL.
type pendingState struct {
	expiresAt time.Time
}

// oauthStateTTL is how long a CSRF state token remains valid.
const oauthStateTTL = 10 * time.Minute

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
			// Return a copy of the cached groups so callers cannot mutate the
			// shared cache entry.
			groupsCopy := make([]string, len(cached.groups))
			copy(groupsCopy, cached.groups)
			return ClusterCredential{
				ImpersonateUser:   cached.username,
				ImpersonateGroups: groupsCopy,
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

	// Copy the groups slice before caching so downstream mutations cannot
	// corrupt the cache entry or cause cross-request data races.
	groupsCopy := make([]string, len(groups))
	copy(groupsCopy, groups)

	m.tokenCache.Store(cacheKey, &cachedIdentity{
		username:  username,
		groups:    groupsCopy,
		expiresAt: time.Now().Add(m.cacheTTL),
	})

	return ClusterCredential{
		ImpersonateUser:   username,
		ImpersonateGroups: groupsCopy,
	}, nil
}

// Login is not supported in openshift mode. Programmatic users send their
// token directly in the Authorization header.
func (m *openShiftManager) Login(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// PasswordLogin authenticates a user with username and password using the
// OpenShift challenge-response flow (same mechanism as `oc login`).
// Uses the built-in openshift-challenging-client OAuth client with implicit
// grant; the token arrives in the Location header fragment of the final redirect.
func (m *openShiftManager) PasswordLogin(ctx context.Context, username, password string) (TokenPair, error) {
	params := url.Values{
		"response_type": {"token"},
		"client_id":     {"openshift-challenging-client"},
	}
	authorizeURL := m.oauthMeta.AuthorizationEndpoint + "?" + params.Encode()

	// Non-redirecting client — we capture the Location header ourselves.
	nonRedirectClient := *m.httpClient
	nonRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	doRequest := func(authHeader string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, authorizeURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-CSRF-Token", "1") // required by OpenShift OAuth
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		return nonRedirectClient.Do(req)
	}

	// Step 1: unauthenticated request to receive the Basic auth challenge.
	resp, err := doRequest("")
	if err != nil {
		return TokenPair{}, fmt.Errorf("openshift: challenge request failed: %w", err)
	}
	resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusFound:
		// Some IDPs skip the challenge and redirect immediately.
		return parseTokenFromLocation(resp.Header.Get("Location"))
	case http.StatusUnauthorized:
		// Expected: proceed to credential submission.
	default:
		return TokenPair{}, fmt.Errorf("openshift: unexpected challenge status %d (IDP may not support challenge-response)", resp.StatusCode)
	}

	// Step 2: retry with Basic credentials.
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	resp2, err := doRequest("Basic " + encoded)
	if err != nil {
		return TokenPair{}, fmt.Errorf("openshift: authenticated challenge request failed: %w", err)
	}
	resp2.Body.Close() //nolint:errcheck

	switch resp2.StatusCode {
	case http.StatusFound:
		return parseTokenFromLocation(resp2.Header.Get("Location"))
	case http.StatusUnauthorized:
		return TokenPair{}, fmt.Errorf("%w: invalid credentials", ErrBadCredentials)
	default:
		return TokenPair{}, fmt.Errorf("openshift: unexpected auth response status %d", resp2.StatusCode)
	}
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

// GetAuthorizeURL builds the OAuth authorize URL and generates a
// cryptographically random CSRF state token (RFC 6749 §10.12). The state is
// stored server-side with a short TTL and must be validated via ValidateState
// when the callback is received. redirectURI overrides the server-configured
// redirect URI when non-empty; callers must validate it before passing it here
// (e.g. localhost-only check in the route handler).
func (m *openShiftManager) GetAuthorizeURL(redirectURI string) (string, string, error) {
	if redirectURI == "" {
		redirectURI = m.cfg.RedirectURI
	}

	state, err := generateState()
	if err != nil {
		return "", "", fmt.Errorf("openshift: generating CSRF state: %w", err)
	}
	m.pendingStates.Store(state, &pendingState{
		expiresAt: time.Now().Add(oauthStateTTL),
	})

	params := url.Values{
		"client_id":     {m.cfg.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(m.cfg.Scopes, " ")},
		"state":         {state},
	}
	return m.oauthMeta.AuthorizationEndpoint + "?" + params.Encode(), state, nil
}

// ValidateState checks that the given state token was issued by
// GetAuthorizeURL, has not expired, and has not already been consumed. On
// success the token is deleted so it cannot be replayed.
func (m *openShiftManager) ValidateState(state string) error {
	val, ok := m.pendingStates.LoadAndDelete(state)
	if !ok {
		return fmt.Errorf("%w: invalid or already-used OAuth state parameter", ErrUnauthenticated)
	}
	ps := val.(*pendingState)
	if time.Now().After(ps.expiresAt) {
		return fmt.Errorf("%w: OAuth state parameter has expired", ErrUnauthenticated)
	}
	return nil
}

// generateState returns a 32-byte hex-encoded cryptographically random string.
func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// OAuthExchange exchanges an OAuth authorization code for tokens. redirectURI
// overrides the server-configured redirect URI when non-empty; callers must
// validate it before passing it here (e.g. localhost-only check in the route
// handler). The redirect URI sent here must exactly match the one used when
// building the authorize URL.
func (m *openShiftManager) OAuthExchange(ctx context.Context, code, redirectURI string) (TokenPair, error) {
	if redirectURI == "" {
		redirectURI = m.cfg.RedirectURI
	}
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

// StartCleanup runs a background goroutine that evicts expired cache entries
// and expired pending CSRF states. Call as: go mgr.StartCleanup(ctx)
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
			m.pendingStates.Range(func(key, value any) bool {
				if ps, ok := value.(*pendingState); ok && now.After(ps.expiresAt) {
					m.pendingStates.Delete(key)
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
		// Only explicit auth failures map to ErrBadCredentials. Server errors
		// and other failures are surfaced as internal errors so that
		// operational incidents are not masked.
		// access_denied: ROPC rejected (wrong credentials, or IDP/client policy).
		// invalid_grant: bad credentials on code exchange or refresh.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			if oauthErr.Error == "invalid_grant" || oauthErr.Error == "access_denied" {
				return TokenPair{}, fmt.Errorf("%w: oauth server returned %q (status %d)",
					ErrBadCredentials, oauthErr.Error, resp.StatusCode)
			}
		}
		return TokenPair{}, fmt.Errorf("openshift: oauth server returned %q (status %d)",
			oauthErr.Error, resp.StatusCode)
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

// parseTokenFromLocation extracts an access_token from an OAuth implicit-grant
// redirect Location header. Checks the URL fragment first, falls back to query params.
func parseTokenFromLocation(location string) (TokenPair, error) {
	if location == "" {
		return TokenPair{}, fmt.Errorf("openshift: no Location header in redirect")
	}
	u, err := url.Parse(location)
	if err != nil {
		return TokenPair{}, fmt.Errorf("openshift: parsing redirect URL: %w", err)
	}
	// Implicit grant puts token in fragment; some OpenShift versions use query params.
	// Fall back to query params only when the fragment carries neither a token
	// nor an error — otherwise we lose error information present in the fragment.
	params, _ := url.ParseQuery(u.Fragment)
	if params.Get("access_token") == "" && params.Get("error") == "" {
		params = u.Query()
	}
	token := params.Get("access_token")
	if token == "" {
		if errCode := params.Get("error"); errCode != "" {
			// Only credential-related errors map to ErrBadCredentials.
			// Server errors (server_error, temporarily_unavailable, etc.) are
			// surfaced as internal errors so operational incidents are not masked.
			if errCode == "access_denied" || errCode == "invalid_grant" {
				return TokenPair{}, fmt.Errorf("%w: %s", ErrBadCredentials, params.Get("error_description"))
			}
			return TokenPair{}, fmt.Errorf("openshift: OAuth error %q: %s", errCode, params.Get("error_description"))
		}
		return TokenPair{}, fmt.Errorf("openshift: no access_token in redirect")
	}
	pair := TokenPair{AccessToken: token}
	if expiresIn, err := strconv.Atoi(params.Get("expires_in")); err == nil && expiresIn > 0 {
		pair.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	return pair, nil
}

// sha256Sum returns a hex-encoded SHA-256 hash of the input string.
func sha256Sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

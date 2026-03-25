package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/dana-team/capp-backend/internal/config"
)

// dexManager implements AuthManager for auth.mode = "dex".
//
// At login time it exchanges username+password for an OIDC ID token via the
// Resource Owner Password Credentials (ROPC) grant against a Dex instance.
// The returned ID token is verified locally using Dex's JWKS endpoint (cached
// by the go-oidc provider). On success a backend-managed JWT session is created
// and a TokenPair is returned to the caller.
//
// Kubernetes API calls use the cluster's pre-configured service-account token —
// the user's Dex identity is NOT forwarded to any cluster. Cluster routing is
// determined entirely by the URL path parameter (/api/v1/clusters/{name}/...).
//
// NOTE: Dex must be configured with a static client that has grantTypes: [password].
type dexManager struct {
	*sessionStore

	cfg      *config.DexConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier

	// httpClient is used for the token endpoint request.
	httpClient *http.Client
}

// newDexManager builds a dexManager. It performs OIDC discovery against
// cfg.Auth.Dex.Endpoint at startup and fails fast if Dex is unreachable.
func newDexManager(cfg *config.Config) (*dexManager, error) {
	httpClient := &http.Client{Timeout: 15 * time.Second}

	// If a custom CA is provided, build a TLS-aware transport.
	if cfg.Auth.Dex.CACert != "" {
		transport, err := buildTLSTransport(cfg.Auth.Dex.CACert)
		if err != nil {
			return nil, fmt.Errorf("dex: building TLS transport: %w", err)
		}
		httpClient.Transport = transport
	}

	// OIDC discovery with an explicit 15-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Inject our httpClient so go-oidc uses our TLS config.
	ctx = oidc.ClientContext(ctx, httpClient)

	provider, err := oidc.NewProvider(ctx, cfg.Auth.Dex.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("dex: OIDC discovery at %q failed: %w", cfg.Auth.Dex.Endpoint, err)
	}

	// ClientID must be explicit so the verifier rejects tokens issued for other clients.
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.Auth.Dex.ClientID})

	return &dexManager{
		sessionStore: newSessionStore(&cfg.Auth.JWT),
		cfg:          &cfg.Auth.Dex,
		provider:     provider,
		verifier:     verifier,
		httpClient:   httpClient,
	}, nil
}

// PasswordLogin exchanges username+password for an OIDC ID token via ROPC,
// verifies the token, creates a backend session, and returns a TokenPair.
func (m *dexManager) PasswordLogin(ctx context.Context, username, password string) (TokenPair, error) {
	// Build the ROPC token request.
	tokenURL := m.provider.Endpoint().TokenURL
	form := url.Values{
		"grant_type":    {"password"},
		"username":      {username},
		"password":      {password},
		"client_id":     {m.cfg.ClientID},
		"client_secret": {m.cfg.ClientSecret},
		"scope":         {strings.Join(m.cfg.Scopes, " ")},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return TokenPair{}, fmt.Errorf("dex: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return TokenPair{}, fmt.Errorf("dex: token request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return TokenPair{}, fmt.Errorf("dex: reading token response: %w", err)
	}

	// Non-2xx from Dex means wrong credentials (invalid_grant) or config error.
	if resp.StatusCode != http.StatusOK {
		// Parse the error field if possible; always map to ErrBadCredentials.
		var dexErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &dexErr)
		return TokenPair{}, fmt.Errorf("%w: dex returned %q", ErrBadCredentials, dexErr.Error)
	}

	// Parse the token response.
	var tokenResp struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return TokenPair{}, fmt.Errorf("dex: parsing token response: %w", err)
	}
	if tokenResp.IDToken == "" {
		return TokenPair{}, fmt.Errorf("dex: token response missing id_token")
	}

	// Verify the ID token using Dex's JWKS endpoint (cached by go-oidc).
	// A verification failure indicates server misconfiguration (wrong issuer,
	// wrong audience, tampered token) — return an internal error, NOT
	// ErrBadCredentials, so a misconfigured Dex is not masked as a wrong password.
	if _, err := m.verifier.Verify(ctx, tokenResp.IDToken); err != nil {
		return TokenPair{}, fmt.Errorf("dex: ID token verification failed: %w", err)
	}

	// Create a backend session with empty clusterToken.
	// The cluster middleware will use the cluster's pre-configured service
	// account token since BearerToken is empty.
	return m.sessionStore.createSession("", "")
}

// Login is not supported in dex mode; use PasswordLogin instead.
func (m *dexManager) Login(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// Authenticate delegates to the shared session store.
func (m *dexManager) Authenticate(_ context.Context, _ string, r *http.Request) (ClusterCredential, error) {
	return m.sessionStore.authenticate(r)
}

// Refresh delegates to the shared session store.
func (m *dexManager) Refresh(_ context.Context, refreshToken string) (TokenPair, error) {
	return m.sessionStore.refresh(refreshToken)
}

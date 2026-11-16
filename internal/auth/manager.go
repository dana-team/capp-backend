// Package auth implements the AuthManager interface and its three concrete
// modes: passthrough, openshift, and ldap.
//
// Mode selection is determined at startup by the auth.mode config value and
// never changes at runtime. All implementations are safe for concurrent use.
//
// Auth modes at a glance:
//
//	passthrough — the client's Kubernetes bearer token is extracted from the
//	              Authorization header and forwarded verbatim to the cluster.
//	              No server-side state is created. Token validation is lazy:
//	              the first K8s API call rejects an invalid token with 401.
//
//	openshift   — authenticates users via the OpenShift OAuth server of the
//	              home cluster (browser Authorization Code flow or direct
//	              bearer token). Tokens are managed by OpenShift, not the
//	              backend (fully stateless). On each request the token is
//	              validated via TokenReview and the user's identity is used
//	              for Kubernetes impersonation on all managed clusters.
//
//	ldap        — authenticates passwords through LDAP search-and-bind and
//	              issues backend access tokens. Tokens are verified locally,
//	              and their identities are used for Kubernetes impersonation
//	              or backend-policy authorization, according to authorization.mode.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"k8s.io/client-go/rest"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

var (
	// ErrUnauthenticated is returned when a request carries no valid credential.
	ErrUnauthenticated = errors.New("request is not authenticated")

	// ErrNotSupported is returned when the current auth mode does not implement
	// the requested operation, including Login/Refresh in passthrough and LDAP modes.
	ErrNotSupported = errors.New("operation not supported in current auth mode")

	// ErrTokenExpired is returned when a token has passed its TTL.
	ErrTokenExpired = errors.New("token has expired")

	// ErrInvalidToken is returned when a token's signature or format is invalid.
	ErrInvalidToken = errors.New("token is invalid")

	// ErrBadCredentials is returned by PasswordLogin when the identity provider
	// rejects the provided username/password combination.
	ErrBadCredentials = errors.New("invalid username or password")

	// ErrDirectoryUnavailable reports directory failures without account diagnostics.
	ErrDirectoryUnavailable = errors.New("directory service unavailable")

	// ErrIdentityUnavailable reports verification failures without reviewer diagnostics.
	ErrIdentityUnavailable = errors.New("identity verification is unavailable")
)

// ── Core types ────────────────────────────────────────────────────────────────

// ClusterCredential holds the caller credential or verified identity used by
// the configured authorization mode.
//
// In passthrough mode BearerToken is taken directly from the Authorization
// header.
//
// In openshift and ldap modes BearerToken is empty and the ImpersonateUser/Groups
// fields are set. Kubernetes authorization uses these fields for impersonation
// with the cluster's service-account token. Backend-policy authorization checks
// the identity locally and uses the backend credential without impersonation.
type ClusterCredential struct {
	BearerToken string

	// ImpersonateUser is the verified username in openshift and ldap auth modes.
	// Kubernetes mode sends it as Impersonate-User; backend-policy matches it locally.
	ImpersonateUser string

	// ImpersonateGroups are the verified groups in openshift and ldap auth modes.
	// Kubernetes mode sends Impersonate-Group headers; backend-policy matches locally.
	ImpersonateGroups []string

	// AuthProvider identifies the provider that verified this identity. It is
	// unset for an unverified passthrough credential.
	AuthProvider string

	// IdentityAuthority identifies the trusted reviewer endpoint or LDAP issuer,
	// with URL user information, query and fragment removed.
	IdentityAuthority string
}

// TokenPair is issued by Login/PasswordLogin and OAuth exchange in openshift mode,
// and by PasswordLogin in ldap mode. LDAP tokens have no refresh token.
type TokenPair struct {
	// AccessToken is the short-lived bearer token sent in the Authorization header of
	// subsequent API calls.
	AccessToken string `json:"accessToken"`

	// RefreshToken is the longer-lived token used to obtain a new TokenPair
	// without re-entering credentials.
	RefreshToken string `json:"refreshToken"`

	// ExpiresAt is the wall-clock time at which AccessToken expires.
	ExpiresAt time.Time `json:"expiresAt"`
}

// AuthManager is the single interface for all authentication operations in
// capp-backend. A single implementation is selected at startup based on the
// auth.mode config value. All methods must be safe for concurrent use.
type AuthManager interface {
	// Authenticate validates the incoming request and returns the
	// ClusterCredential used for Kubernetes client construction or backend-policy
	// authorization, according to the configured authorization mode.
	//
	// Returns ErrUnauthenticated if the request carries no valid credential.
	// In ldap mode, returns ErrTokenExpired for an expired access token.
	// Backend-policy review failures return ErrIdentityUnavailable without
	// exposing upstream diagnostics; rejected identities return ErrUnauthenticated.
	Authenticate(ctx context.Context, clusterName string, r *http.Request) (ClusterCredential, error)

	// Login validates a raw Kubernetes bearer token against the named cluster
	// and, on success, returns a TokenPair (openshift mode only).
	//
	// Returns ErrNotSupported in passthrough and ldap modes.
	Login(ctx context.Context, clusterName string, token string) (TokenPair, error)

	// PasswordLogin authenticates a user with username and password against an
	// external identity provider (openshift and ldap modes).
	//
	// Returns ErrNotSupported in passthrough mode.
	// Returns ErrBadCredentials if the provider rejects the credentials.
	// In ldap mode, returns ErrDirectoryUnavailable for directory failures.
	PasswordLogin(ctx context.Context, username, password string) (TokenPair, error)

	// Refresh exchanges a valid refresh token for a new TokenPair (openshift mode only).
	//
	// Returns ErrNotSupported in passthrough and ldap modes.
	Refresh(ctx context.Context, refreshToken string) (TokenPair, error)
}

// OAuthAuthorizer is an optional interface implemented by auth managers that
// support the OAuth Authorization Code flow. Currently only openShiftManager
// implements it. Route handlers type-assert to this interface to expose the
// /openshift/authorize and /openshift/callback endpoints.
type OAuthAuthorizer interface {
	// GetAuthorizeURL returns the OAuth authorization URL and a CSRF state
	// token. The state token is stateless (self-contained, signed) and must be
	// validated via ValidateState during the callback. redirectURI overrides
	// the server-configured redirect URI when non-empty (localhost URIs only).
	GetAuthorizeURL(redirectURI string) (authorizeURL string, state string, err error)

	// ValidateState checks that the given state token was issued by
	// GetAuthorizeURL (valid signature) and has not expired. Validation is
	// stateless, so any backend replica can validate a token issued by another.
	ValidateState(state string) error

	// OAuthExchange exchanges an OAuth authorization code for an access token
	// and refresh token from the identity provider. redirectURI overrides the
	// server-configured redirect URI when non-empty (localhost URIs only).
	OAuthExchange(ctx context.Context, code, redirectURI string) (TokenPair, error)
}

// ── Factory ───────────────────────────────────────────────────────────────────

// Dependencies supplies trusted clients independently of caller credentials.
// A nil HomeConfig uses the workload's in-cluster OpenShift review configuration.
type Dependencies struct {
	TargetReviewers map[string]TokenReviewer
	HomeConfig      *rest.Config
}

// New instantiates the AuthManager implementation selected by cfg.Auth.Mode.
//
// For openshift mode, callers must also invoke the returned manager's StartCleanup
// method (if the concrete type implements it) to start background cleanup tasks.
func New(cfg *config.Config, dependencies Dependencies) (AuthManager, error) {
	switch cfg.Auth.Mode {
	case config.AuthModePassthrough:
		if cfg.Authorization.Mode == config.AuthorizationModeBackendPolicy {
			return newVerifiedPassthroughManager(cfg.Clusters, dependencies.TargetReviewers)
		}
		return newPassthroughManager(), nil

	case config.AuthModeOpenShift:
		if dependencies.HomeConfig != nil {
			return newOpenShiftManagerForConfig(cfg, dependencies.HomeConfig)
		}
		return newOpenShiftManager(cfg)

	case config.AuthModeLDAP:
		return newLDAPManager(cfg.Auth.LDAP)

	default:
		// Validate() should catch this before we reach New(), but guard anyway.
		return nil, fmt.Errorf("auth: unknown mode %q", cfg.Auth.Mode)
	}
}

// sanitizeAuthority prepares trusted configuration for identity metadata.
// Non-URL issuers are retained; unparseable URLs are omitted rather than leaked.
func sanitizeAuthority(authority string) string {
	// Plain issuers need not follow URL escaping rules.
	if !strings.Contains(authority, ":") && !strings.HasPrefix(authority, "//") {
		return authority
	}
	u, err := url.Parse(authority)
	if err != nil {
		return ""
	}
	if u.Scheme == "" && u.Host == "" {
		return authority
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

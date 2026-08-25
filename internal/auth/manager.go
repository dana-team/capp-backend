// Package auth implements the AuthManager interface and its two concrete
// modes: passthrough and openshift.
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
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

var (
	// ErrUnauthenticated is returned when a request carries no valid credential.
	ErrUnauthenticated = errors.New("request is not authenticated")

	// ErrNotSupported is returned by Login/Refresh when the current auth mode
	// does not implement token management (passthrough).
	ErrNotSupported = errors.New("operation not supported in current auth mode")

	// ErrTokenExpired is returned when a token has passed its TTL.
	ErrTokenExpired = errors.New("token has expired")

	// ErrInvalidToken is returned when a token's signature or format is invalid.
	ErrInvalidToken = errors.New("token is invalid")

	// ErrBadCredentials is returned by PasswordLogin when the identity provider
	// rejects the provided username/password combination.
	ErrBadCredentials = errors.New("invalid username or password")
)

// ── Core types ────────────────────────────────────────────────────────────────

// ClusterCredential holds the information needed to authenticate and authorize
// a Kubernetes API request on behalf of an incoming user.
//
// In passthrough mode BearerToken is taken directly from the Authorization
// header.
//
// In openshift mode BearerToken is empty and the ImpersonateUser/Groups
// fields are set. The cluster's service-account token is used for
// authentication while impersonation headers enforce the user's RBAC identity.
type ClusterCredential struct {
	BearerToken string

	// ImpersonateUser is the username to impersonate via the
	// Impersonate-User header. Set only in openshift auth mode.
	ImpersonateUser string

	// ImpersonateGroups are the groups to impersonate via
	// Impersonate-Group headers. Set only in openshift auth mode.
	ImpersonateGroups []string
}

// TokenPair is issued by Login/PasswordLogin and OAuth exchange in openshift auth mode.
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
	// ClusterCredential that the cluster middleware will use to build a scoped
	// Kubernetes client for the named cluster.
	//
	// Returns ErrUnauthenticated if the request carries no valid credential.
	Authenticate(ctx context.Context, clusterName string, r *http.Request) (ClusterCredential, error)

	// Login validates a raw Kubernetes bearer token against the named cluster
	// and, on success, returns a TokenPair (openshift mode only).
	//
	// Returns ErrNotSupported in passthrough mode.
	Login(ctx context.Context, clusterName string, token string) (TokenPair, error)

	// PasswordLogin authenticates a user with username and password against an
	// external identity provider (openshift mode only).
	//
	// Returns ErrNotSupported in passthrough mode.
	// Returns ErrBadCredentials if the provider rejects the credentials.
	PasswordLogin(ctx context.Context, username, password string) (TokenPair, error)

	// Refresh exchanges a valid refresh token for a new TokenPair (openshift mode only).
	//
	// Returns ErrNotSupported in passthrough mode.
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

// New instantiates the AuthManager implementation selected by cfg.Auth.Mode.
//
// For openshift mode, callers must also invoke the returned manager's StartCleanup
// method (if the concrete type implements it) to start background cleanup tasks.
func New(cfg *config.Config) (AuthManager, error) {
	switch cfg.Auth.Mode {
	case "passthrough":
		return newPassthroughManager(), nil

	case "openshift":
		return newOpenShiftManager(cfg)

	default:
		// Validate() should catch this before we reach New(), but guard anyway.
		return nil, fmt.Errorf("auth: unknown mode %q", cfg.Auth.Mode)
	}
}

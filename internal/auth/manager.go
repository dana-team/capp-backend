// Package auth implements the AuthManager interface and its three concrete
// modes: passthrough, jwt, and static.
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
//	jwt         — POST /api/v1/auth/login accepts a cluster name + raw token.
//	              The backend validates the token, stores it server-side keyed
//	              by a random session ID, and issues short-lived JWTs. The
//	              cluster token is never sent over the wire again.
//
//	static      — a hard-coded list of API keys, for development/CI only.
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
	// does not implement token management (passthrough, static).
	ErrNotSupported = errors.New("operation not supported in current auth mode")

	// ErrTokenExpired is returned when a JWT or session entry has passed its TTL.
	ErrTokenExpired = errors.New("token has expired")

	// ErrInvalidToken is returned when a JWT signature or format is invalid.
	ErrInvalidToken = errors.New("token is invalid")
)

// ── Core types ────────────────────────────────────────────────────────────────

// ClusterCredential holds the bearer token used to authenticate against a
// specific Kubernetes API server on behalf of an incoming request.
//
// In passthrough mode this value is taken directly from the Authorization
// header. In jwt mode it is retrieved from the server-side session store.
// In static mode it is empty (the cluster is expected to permit unauthenticated
// access or use the token configured in the cluster's credential block).
type ClusterCredential struct {
	BearerToken string
}

// TokenPair is issued by Login and Refresh in jwt auth mode.
type TokenPair struct {
	// AccessToken is the short-lived JWT sent in the Authorization header of
	// subsequent API calls.
	AccessToken string `json:"accessToken"`

	// RefreshToken is the longer-lived JWT used to obtain a new TokenPair
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
	// and, on success, returns a TokenPair (jwt mode only).
	//
	// Returns ErrNotSupported in passthrough and static modes.
	Login(ctx context.Context, clusterName string, token string) (TokenPair, error)

	// Refresh exchanges a valid refresh token for a new TokenPair (jwt mode only).
	//
	// Returns ErrNotSupported in passthrough and static modes.
	Refresh(ctx context.Context, refreshToken string) (TokenPair, error)
}

// ── Factory ───────────────────────────────────────────────────────────────────

// New instantiates the AuthManager implementation selected by cfg.Auth.Mode.
//
// For jwt mode, callers must also invoke the returned manager's StartCleanup
// method (if the concrete type implements it) to start the background session
// garbage collector.
func New(cfg *config.Config) (AuthManager, error) {
	switch cfg.Auth.Mode {
	case "passthrough":
		return newPassthroughManager(), nil

	case "jwt":
		// Build a map of clusterName → apiServerURL so the JWT manager can
		// validate tokens against the correct cluster endpoint.
		clusterURLs := make(map[string]string, len(cfg.Clusters))
		for _, c := range cfg.Clusters {
			if c.Credential.Inline != nil {
				clusterURLs[c.Name] = c.Credential.Inline.APIServer
			}
			// For kubeconfig-based clusters the URL is parsed at runtime by
			// the cluster loader; jwt validation for those clusters falls back
			// to skipping the /version probe (the K8s API will reject the
			// token itself on the first real call).
		}
		return newJWTManager(&cfg.Auth.JWT, clusterURLs), nil

	case "static":
		return newStaticManager(cfg.Auth.Static.APIKeys), nil

	default:
		// Validate() should catch this before we reach New(), but guard anyway.
		return nil, fmt.Errorf("auth: unknown mode %q", cfg.Auth.Mode)
	}
}

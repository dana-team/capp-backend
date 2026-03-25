package auth

import (
	"context"
	"net/http"
	"strings"
)

// passthroughManager implements AuthManager for auth.mode = "passthrough".
//
// Design decision: token validation is intentionally lazy.
//
// In passthrough mode the client's Kubernetes bearer token is extracted from
// the Authorization header and stored in a ClusterCredential. It is NOT
// validated here by probing the cluster's /version endpoint. Instead,
// validation happens implicitly on the first real Kubernetes API call: if the
// token is invalid the K8s API server returns 401, which the cluster middleware
// translates into an API error and returns to the client.
//
// This avoids an extra round-trip to the cluster on every request while still
// providing the same security guarantee: an invalid token is rejected before
// any data is read or written.
type passthroughManager struct{}

// newPassthroughManager returns a ready-to-use passthroughManager.
func newPassthroughManager() *passthroughManager {
	return &passthroughManager{}
}

// Authenticate extracts the bearer token from the Authorization header.
// Returns ErrUnauthenticated if the header is absent or not in Bearer format.
func (m *passthroughManager) Authenticate(_ context.Context, _ string, r *http.Request) (ClusterCredential, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ClusterCredential{}, ErrUnauthenticated
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return ClusterCredential{}, ErrUnauthenticated
	}

	token := strings.TrimPrefix(raw, prefix)
	if token == "" {
		return ClusterCredential{}, ErrUnauthenticated
	}

	return ClusterCredential{BearerToken: token}, nil
}

// Login is not supported in passthrough mode. The client manages its own
// Kubernetes token; the backend has no session concept.
func (m *passthroughManager) Login(_ context.Context, _ string, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// PasswordLogin is not supported in passthrough mode.
func (m *passthroughManager) PasswordLogin(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// Refresh is not supported in passthrough mode.
func (m *passthroughManager) Refresh(_ context.Context, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

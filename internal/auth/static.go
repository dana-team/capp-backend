package auth

import (
	"context"
	"net/http"
	"strings"
)

// staticManager implements AuthManager for auth.mode = "static".
//
// This mode is intended exclusively for development and integration testing
// where a real Kubernetes cluster may not be available or may not enforce
// authentication. A fixed set of API keys is loaded from the config file and
// matched against the Authorization: Bearer header on each request.
//
// The ClusterCredential returned always has an empty BearerToken because
// static mode assumes the cluster is either unauthenticated (e.g. a local
// kind cluster with --disable-modifier=true) or that a per-cluster token is
// baked into the cluster credential block in config.
//
// DO NOT use this mode in production.
type staticManager struct {
	// keys is a set of valid API key strings. Using a map gives O(1) lookup.
	keys map[string]struct{}
}

// newStaticManager builds a staticManager from the provided key list.
func newStaticManager(apiKeys []string) *staticManager {
	keys := make(map[string]struct{}, len(apiKeys))
	for _, k := range apiKeys {
		if k != "" {
			keys[k] = struct{}{}
		}
	}
	return &staticManager{keys: keys}
}

// Authenticate checks whether the bearer token in the Authorization header
// matches one of the configured API keys. Returns ErrUnauthenticated if the
// header is absent, malformed, or the key is not recognised.
func (m *staticManager) Authenticate(_ context.Context, _ string, r *http.Request) (ClusterCredential, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ClusterCredential{}, ErrUnauthenticated
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return ClusterCredential{}, ErrUnauthenticated
	}

	key := strings.TrimPrefix(raw, prefix)
	if _, ok := m.keys[key]; !ok {
		return ClusterCredential{}, ErrUnauthenticated
	}

	// Return an empty BearerToken: in static mode the cluster credential
	// comes from the cluster config, not from the client.
	return ClusterCredential{}, nil
}

// Login is not supported in static mode.
func (m *staticManager) Login(_ context.Context, _ string, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// PasswordLogin is not supported in static mode.
func (m *staticManager) PasswordLogin(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// Refresh is not supported in static mode.
func (m *staticManager) Refresh(_ context.Context, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

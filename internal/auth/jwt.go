package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
)

// jwtManager implements AuthManager for auth.mode = "jwt".
//
// Sessions are stored in an in-memory sync.Map via the embedded sessionStore.
// This is sufficient for single replica deployments. For multi-replica
// deployments, replace the sync.Map with a Redis-backed store — the
// sessionEntry type is designed to be easily serialisable.
type jwtManager struct {
	*sessionStore

	// clusterURLs maps cluster name → API server URL for token validation at
	// login time. Populated once at startup from the clusters config.
	clusterURLs map[string]string

	// httpClient is used for the /version token validation probe at login.
	httpClient *http.Client
}

// newJWTManager returns a jwtManager. Call StartCleanup in a goroutine to
// enable background expiry of stale sessions.
func newJWTManager(cfg *config.JWTConfig, clusterURLs map[string]string) *jwtManager {
	return &jwtManager{
		sessionStore: newSessionStore(cfg),
		clusterURLs:  clusterURLs,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Login validates token against the named cluster's /version endpoint, creates
// a session, and returns a TokenPair. Returns ErrUnauthenticated if the token
// is rejected by the cluster.
func (m *jwtManager) Login(ctx context.Context, clusterName string, token string) (TokenPair, error) {
	if err := m.validateTokenAgainstCluster(ctx, clusterName, token); err != nil {
		return TokenPair{}, err
	}
	return m.createSession(clusterName, token)
}

// PasswordLogin is not supported in jwt mode; it always returns ErrNotSupported.
func (m *jwtManager) PasswordLogin(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// Authenticate, Refresh, and StartCleanup are promoted from the embedded
// *sessionStore. The AuthManager interface is satisfied via the wrapper methods
// below that adapt the sessionStore signatures to the interface signatures.

// Authenticate implements AuthManager by delegating to the embedded sessionStore.
func (m *jwtManager) Authenticate(_ context.Context, _ string, r *http.Request) (ClusterCredential, error) {
	return m.authenticate(r)
}

// Refresh implements AuthManager by delegating to the embedded sessionStore.
func (m *jwtManager) Refresh(_ context.Context, refreshToken string) (TokenPair, error) {
	return m.refresh(refreshToken)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// validateTokenAgainstCluster probes the cluster's /version endpoint with the
// provided token. A 200 response indicates the token is accepted; anything
// else returns ErrUnauthenticated.
//
// If the cluster URL is not known (kubeconfig-based clusters), validation is
// skipped — the Kubernetes API server itself will reject invalid tokens on the
// first real call.
func (m *jwtManager) validateTokenAgainstCluster(ctx context.Context, clusterName, token string) error {
	apiServer, ok := m.clusterURLs[clusterName]
	if !ok || apiServer == "" {
		// Cannot probe — skip validation. The cluster client will reject
		// invalid tokens on the first real K8s API call.
		return nil
	}

	url := strings.TrimRight(apiServer, "/") + "/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: building validation request: %s", ErrUnauthenticated, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: cluster probe failed: %s", ErrUnauthenticated, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: cluster returned status %d for token validation", ErrUnauthenticated, resp.StatusCode)
	}
	return nil
}

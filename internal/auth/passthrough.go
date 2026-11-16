package auth

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/internal/config"
	authenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

// passthroughManager implements AuthManager for auth.mode = "passthrough".
//
// Kubernetes mode forwards the token for validation by resource calls.
// Backend-policy mode verifies it with the requested target before policy checks.
// Verified identities are never cached by this manager.
type passthroughManager struct {
	reviewers map[string]TokenReviewer
}

// TokenReviewer supplies a target's existing backend authentication client and
// its trusted API endpoint. The transport must verify TLS and reject redirects.
type TokenReviewer struct {
	Client    authenticationv1.AuthenticationV1Interface
	Authority string
}

// newPassthroughManager returns a ready-to-use passthroughManager.
func newPassthroughManager() *passthroughManager {
	return &passthroughManager{}
}

func newVerifiedPassthroughManager(clusters []config.ClusterConfig, reviewers map[string]TokenReviewer) (*passthroughManager, error) {
	m := &passthroughManager{reviewers: make(map[string]TokenReviewer, len(clusters))}
	for _, cluster := range clusters {
		reviewer := reviewers[cluster.Name]
		if reviewer.Client == nil {
			return nil, fmt.Errorf("passthrough: missing reviewer for cluster %q", cluster.Name)
		}
		reviewer.Authority = sanitizeAuthority(reviewer.Authority)
		m.reviewers[cluster.Name] = reviewer
	}
	return m, nil
}

// Authenticate extracts the bearer token from the Authorization header.
// Returns ErrUnauthenticated if the header is absent or not in Bearer format.
func (m *passthroughManager) Authenticate(ctx context.Context, clusterName string, r *http.Request) (ClusterCredential, error) {
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
	if m.reviewers != nil {
		if strings.ContainsAny(token, " \t\r\n") {
			return ClusterCredential{}, ErrUnauthenticated
		}
		reviewer, ok := m.reviewers[clusterName]
		if !ok {
			// No identity can be verified for an unknown target.
			return ClusterCredential{}, ErrUnauthenticated
		}
		result, err := reviewToken(ctx, reviewer.Client.RESTClient(), token)
		if err != nil {
			return ClusterCredential{}, err
		}
		if err := validateTokenReview(result); err != nil {
			return ClusterCredential{}, err
		}
		return ClusterCredential{
			ImpersonateUser: result.Status.User.Username, ImpersonateGroups: slices.Clone(result.Status.User.Groups),
			AuthProvider: config.AuthModePassthrough, IdentityAuthority: reviewer.Authority,
		}, nil
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

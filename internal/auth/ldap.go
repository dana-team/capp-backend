package auth

import (
	"context"
	"net/http"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/golang-jwt/jwt/v5/request"
)

// ldapManager authenticates passwords through LDAP and verifies access tokens locally.
type ldapManager struct {
	directory *ldapDirectory
	tokens    *ldapTokens
}

func newLDAPManager(cfg config.LDAPConfig) (*ldapManager, error) {
	directory, err := newLDAPDirectory(cfg)
	if err != nil {
		return nil, err
	}
	tokens, err := newLDAPTokens(cfg)
	if err != nil {
		return nil, err
	}
	return &ldapManager{directory: directory, tokens: tokens}, nil
}

// Authenticate verifies a backend-issued bearer token without contacting LDAP.
func (m *ldapManager) Authenticate(_ context.Context, _ string, r *http.Request) (ClusterCredential, error) {
	token, err := (request.BearerExtractor{}).ExtractToken(r)
	if err != nil {
		return ClusterCredential{}, ErrUnauthenticated
	}
	return m.tokens.verify(token)
}

// Login is unsupported because raw Kubernetes tokens cannot authenticate LDAP users.
func (m *ldapManager) Login(_ context.Context, _, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

// PasswordLogin issues an access token after password verification and group resolution.
func (m *ldapManager) PasswordLogin(ctx context.Context, username, password string) (TokenPair, error) {
	canonical, groups, err := m.directory.verify(ctx, username, password)
	if err != nil {
		return TokenPair{}, err
	}
	return m.tokens.issue(canonical, groups)
}

// Refresh is unsupported; LDAP users must log in again after their access token expires.
func (m *ldapManager) Refresh(_ context.Context, _ string) (TokenPair, error) {
	return TokenPair{}, ErrNotSupported
}

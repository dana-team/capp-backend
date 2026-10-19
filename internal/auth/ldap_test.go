package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/go-ldap/ldap/v3"
	"github.com/google/go-cmp/cmp"
)

func TestNewLDAPManager(t *testing.T) {
	cfg, _ := tokenConfig(t, "active")
	for _, tc := range []struct {
		name   string
		change func(*config.LDAPConfig)
	}{
		{name: "invalid directory configuration", change: func(c *config.LDAPConfig) { c.URL = "://invalid" }},
		{name: "invalid signing configuration", change: func(c *config.LDAPConfig) { c.SigningPrivateKey = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := cfg
			tc.change(&input)
			if _, err := New(&config.Config{Auth: config.AuthConfig{Mode: config.AuthModeLDAP, LDAP: input}}); err == nil {
				t.Fatal("New(ldap) accepted invalid configuration")
			}
		})
	}
}

func TestLDAPManagerPasswordLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups []string
	}{
		{name: "canonical identity with groups", groups: []string{"Developers"}},
		{name: "canonical identity without groups", groups: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLDAPAuthManager(t)
			search, user := directoryClients()
			if len(tc.groups) == 0 {
				search.groupResult = &ldap.SearchResult{}
			}
			password := " secret\r\n "
			user.bind = func(dn, supplied string) error {
				if dn != "uid=alice,dc=example" || supplied != password {
					t.Fatal("PasswordLogin did not preserve the directory DN and password bytes")
				}
				return nil
			}
			m.directory = mockDirectory(t, m.directory.cfg, search, user)
			m.tokens.now = func() time.Time {
				if user.binds != 1 || search.groupCalls != 1 || !search.closed || !user.closed {
					t.Fatal("token issuance started before directory verification and cleanup completed")
				}
				return time.Now()
			}
			pair, err := m.PasswordLogin(t.Context(), "alice", password)
			if err != nil {
				t.Fatalf("PasswordLogin() error = %v", err)
			}
			if pair.AccessToken == "" || pair.RefreshToken != "" || pair.ExpiresAt.IsZero() {
				t.Fatal("PasswordLogin() must return an access token and expiry without a refresh token")
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
			credential, err := m.Authenticate(t.Context(), "another-cluster", req)
			if err != nil {
				t.Fatalf("Authenticate(issued token) error = %v", err)
			}
			want := ClusterCredential{ImpersonateUser: "Alice", ImpersonateGroups: tc.groups}
			if diff := cmp.Diff(want, credential); diff != "" {
				t.Errorf("Authenticate() mismatch (-want +got):\n%s", diff)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		username string
		password string
		change   func(*directoryClient, *directoryClient)
		want     error
	}{
		{name: "empty username", password: "secret", want: ErrBadCredentials},
		{name: "empty password", username: "alice", want: ErrBadCredentials},
		{name: "unknown user", username: "alice", password: "secret", change: func(s, _ *directoryClient) {
			s.result = &ldap.SearchResult{}
		}, want: ErrBadCredentials},
		{name: "rejected password", username: "alice", password: "secret", change: func(_, u *directoryClient) {
			u.bind = func(string, string) error {
				return ldap.NewError(ldap.LDAPResultInvalidCredentials, errors.New("private diagnostic"))
			}
		}, want: ErrBadCredentials},
		{name: "failed group resolution", username: "alice", password: "secret", change: func(s, _ *directoryClient) {
			s.groupError = errors.New("private diagnostic")
		}, want: ErrDirectoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLDAPAuthManager(t)
			search, user := directoryClients()
			if tc.change != nil {
				tc.change(search, user)
			}
			m.directory = mockDirectory(t, m.directory.cfg, search, user)
			m.tokens.now = func() time.Time {
				t.Fatal("PasswordLogin attempted token issuance after failed authentication")
				return time.Time{}
			}
			pair, err := m.PasswordLogin(t.Context(), tc.username, tc.password)
			if !errors.Is(err, tc.want) {
				t.Fatalf("PasswordLogin() error = %v, want %v", err, tc.want)
			}
			if diff := cmp.Diff(TokenPair{}, pair); diff != "" {
				t.Errorf("failed PasswordLogin() returned a token (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("caller cancellation", func(t *testing.T) {
		m := newLDAPAuthManager(t)
		m.directory.connect = func(context.Context) (ldapClient, func(), error) {
			t.Fatal("cancelled PasswordLogin attempted a directory connection")
			return nil, nil, ErrDirectoryUnavailable
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := m.PasswordLogin(ctx, "alice", "secret"); !errors.Is(err, ErrDirectoryUnavailable) {
			t.Errorf("PasswordLogin(cancelled) = %v, want ErrDirectoryUnavailable", err)
		}
	})
}

func TestLDAPManagerAuthenticate(t *testing.T) {
	m := newLDAPAuthManager(t)
	m.directory.connect = func(context.Context) (ldapClient, func(), error) {
		t.Fatal("Authenticate attempted a directory connection")
		return nil, nil, ErrDirectoryUnavailable
	}
	pair, err := m.tokens.issue("Alice", []string{"Developers"})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("mixed-case bearer scheme", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "bEaReR "+pair.AccessToken)
		credential, err := m.Authenticate(t.Context(), "any-cluster", req)
		if err != nil {
			t.Fatalf("Authenticate(mixed-case bearer) error = %v", err)
		}
		want := ClusterCredential{ImpersonateUser: "Alice", ImpersonateGroups: []string{"Developers"}}
		if diff := cmp.Diff(want, credential); diff != "" {
			t.Errorf("Authenticate() mismatch (-want +got):\n%s", diff)
		}
	})
	m.tokens.now = func() time.Time { return pair.ExpiresAt }
	for _, tc := range []struct {
		name   string
		header string
		want   error
	}{
		{name: "missing header", want: ErrUnauthenticated},
		{name: "wrong scheme", header: "Basic " + pair.AccessToken, want: ErrUnauthenticated},
		{name: "empty bearer", header: "Bearer ", want: ErrUnauthenticated},
		{name: "invalid token", header: "Bearer invalid", want: ErrUnauthenticated},
		{name: "expired token", header: "Bearer " + pair.AccessToken, want: ErrTokenExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", tc.header)
			credential, err := m.Authenticate(t.Context(), "any-cluster", req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tc.want)
			}
			if diff := cmp.Diff(ClusterCredential{}, credential); diff != "" {
				t.Errorf("failed Authenticate() returned credentials (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLDAPManagerLogin(t *testing.T) {
	m := newLDAPAuthManager(t)
	pair, err := m.Login(t.Context(), "cluster", "raw-token")
	if !errors.Is(err, ErrNotSupported) || pair != (TokenPair{}) {
		t.Errorf("Login() = %v, %v; want empty token pair and ErrNotSupported", pair, err)
	}
}

func TestLDAPManagerRefresh(t *testing.T) {
	m := newLDAPAuthManager(t)
	pair, err := m.Refresh(t.Context(), "refresh-token")
	if !errors.Is(err, ErrNotSupported) || pair != (TokenPair{}) {
		t.Errorf("Refresh() = %v, %v; want empty token pair and ErrNotSupported", pair, err)
	}
}

func newLDAPAuthManager(t *testing.T) *ldapManager {
	t.Helper()
	cfg := directoryConfig()
	keys, _ := tokenConfig(t, "active")
	cfg.SigningKeyID = keys.SigningKeyID
	cfg.SigningPrivateKey = keys.SigningPrivateKey
	cfg.Issuer = keys.Issuer
	cfg.Audience = keys.Audience
	cfg.TokenTTLSeconds = keys.TokenTTLSeconds
	manager, err := New(&config.Config{Auth: config.AuthConfig{Mode: config.AuthModeLDAP, LDAP: cfg}})
	if err != nil {
		t.Fatalf("New(ldap) error = %v", err)
	}
	m, ok := manager.(*ldapManager)
	if !ok {
		t.Fatalf("New(ldap) returned %T, want *ldapManager", manager)
	}
	return m
}

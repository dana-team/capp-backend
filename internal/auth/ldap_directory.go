package auth

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/go-ldap/ldap/v3"
)

// ldapDirectory retains configuration only; each verification owns its connections.
type ldapDirectory struct {
	cfg     config.LDAPConfig
	connect func(context.Context) (ldapClient, func(), error)
}

// ldapClient contains the directory operations required for credential verification.
// Connection setup and cleanup remain the connector's responsibility.
type ldapClient interface {
	Bind(string, string) error
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
	SearchWithPaging(*ldap.SearchRequest, uint32) (*ldap.SearchResult, error)
}

func newLDAPDirectory(cfg config.LDAPConfig) (*ldapDirectory, error) {
	endpoint, err := url.Parse(cfg.URL)
	if err != nil || endpoint.Scheme != "ldaps" || endpoint.Hostname() == "" {
		return nil, ErrDirectoryUnavailable
	}
	tlsConfig, err := buildTLSConfig(cfg.CACert)
	if err != nil {
		return nil, ErrDirectoryUnavailable
	}
	tlsConfig.ServerName = endpoint.Hostname()
	port := endpoint.Port()
	if port == "" {
		port = ldap.DefaultLdapsPort
	}
	address := net.JoinHostPort(endpoint.Hostname(), port)
	return &ldapDirectory{
		cfg: cfg,
		connect: func(ctx context.Context) (ldapClient, func(), error) {
			return connectLDAP(ctx, address, tlsConfig)
		},
	}, nil
}

func (d *ldapDirectory) verify(ctx context.Context, username, password string) (string, []string, error) {
	if username == "" || password == "" {
		return "", nil, ErrBadCredentials
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return "", nil, ErrDirectoryUnavailable
	}
	search, closeSearch, err := d.connect(ctx)
	if err != nil {
		return "", nil, ErrDirectoryUnavailable
	}
	defer closeSearch()
	if err := search.Bind(d.cfg.BindDN, d.cfg.BindPassword); err != nil || ctx.Err() != nil {
		return "", nil, ErrDirectoryUnavailable
	}
	request := ldap.NewSearchRequest(d.cfg.UserBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false, strings.ReplaceAll(d.cfg.UserFilter, "{username}", ldap.EscapeFilter(username)),
		[]string{d.cfg.UsernameAttribute}, nil)
	result, err := search.Search(request)
	if err != nil || ctx.Err() != nil || !completeLDAPResult(result) {
		return "", nil, ErrDirectoryUnavailable
	}
	if len(result.Entries) != 1 {
		return "", nil, ErrBadCredentials
	}
	user := result.Entries[0]
	canonical, err := ldapIdentity(user, d.cfg.UsernameAttribute)
	if err != nil {
		return "", nil, err
	}
	if dn, err := ldap.ParseDN(user.DN); err != nil || len(dn.RDNs) == 0 {
		return "", nil, ErrDirectoryUnavailable
	}
	if err := d.verifyPassword(ctx, user.DN, password); err != nil {
		return "", nil, err
	}
	groups, err := d.groups(ctx, search, user.DN)
	if err != nil {
		return "", nil, err
	}
	return canonical, groups, nil
}

func (d *ldapDirectory) verifyPassword(ctx context.Context, dn, password string) error {
	client, closeClient, err := d.connect(ctx)
	if err != nil {
		return ErrDirectoryUnavailable
	}
	defer closeClient()
	err = client.Bind(dn, password)
	if ctx.Err() != nil {
		return ErrDirectoryUnavailable
	}
	if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
		return ErrBadCredentials
	}
	if err != nil {
		return ErrDirectoryUnavailable
	}
	return nil
}

func (d *ldapDirectory) groups(ctx context.Context, client ldapClient, dn string) ([]string, error) {
	request := ldap.NewSearchRequest(d.cfg.GroupBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false, strings.ReplaceAll(d.cfg.GroupFilter, "{userDN}", ldap.EscapeFilter(dn)),
		[]string{d.cfg.GroupNameAttribute}, nil)
	result, err := client.SearchWithPaging(request, 500)
	if err != nil || ctx.Err() != nil || !completeLDAPResult(result) {
		return nil, ErrDirectoryUnavailable
	}
	groups := make([]string, 0, len(result.Entries))
	seen := make(map[string]struct{}, len(result.Entries))
	for _, entry := range result.Entries {
		group, err := ldapIdentity(entry, d.cfg.GroupNameAttribute)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[group]; !ok {
			groups = append(groups, group)
			seen[group] = struct{}{}
		}
	}
	return groups, nil
}

func completeLDAPResult(result *ldap.SearchResult) bool {
	return result != nil && result.ResultCode == ldap.LDAPResultSuccess && len(result.Referrals) == 0
}

func ldapIdentity(entry *ldap.Entry, attribute string) (string, error) {
	if entry == nil {
		return "", ErrDirectoryUnavailable
	}
	values := entry.GetEqualFoldAttributeValues(attribute)
	if len(values) != 1 || !validLDAPIdentity(values[0]) {
		return "", ErrDirectoryUnavailable
	}
	return values[0], nil
}

func validLDAPIdentity(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.HasPrefix(value, "system:") &&
		utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

// connectLDAP returns an encrypted client owned by the caller. On success, the
// caller must invoke the returned cleanup function, which stops cancellation
// handling and closes the socket and LDAP client. Errors leave no client to clean up.
func connectLDAP(ctx context.Context, address string, tlsConfig *tls.Config) (ldapClient, func(), error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, ErrDirectoryUnavailable
	}
	// Closing the socket interrupts both the TLS handshake and LDAP operations.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	closeRaw := func() {
		stop()
		_ = raw.Close()
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := raw.SetDeadline(deadline); err != nil {
			closeRaw()
			return nil, nil, ErrDirectoryUnavailable
		}
	}
	secure := tls.Client(raw, tlsConfig)
	if err := secure.HandshakeContext(ctx); err != nil {
		closeRaw()
		return nil, nil, ErrDirectoryUnavailable
	}
	client := ldap.NewConn(secure, true)
	client.Start()
	closeClient := func() {
		closeRaw()
		_ = client.Close()
	}
	if ctx.Err() != nil {
		closeClient()
		return nil, nil, ErrDirectoryUnavailable
	}
	return client, closeClient, nil
}

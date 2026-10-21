package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLDAPDirectoryVerify(t *testing.T) {
	cfg := directoryConfig()
	dn := "CN=Alice*(admin),DC=example,DC=com"
	user := &directoryClient{bind: func(name, password string) error {
		assert.Equal(t, dn, name)
		assert.Equal(t, " secret\r\n\x00 ", password)
		return nil
	}}
	search := &directoryClient{
		bind: func(name, password string) error {
			assert.Equal(t, cfg.BindDN, name)
			assert.Equal(t, cfg.BindPassword, password)
			return nil
		},
		search: func(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
			assert.Equal(t, "(uid=alice\\2a\\29\\28uid=\\2a\\29\\00)", req.Filter)
			assert.Equal(t, cfg.UserBaseDN, req.BaseDN)
			assert.Equal(t, ldap.ScopeWholeSubtree, req.Scope)
			assert.Equal(t, ldap.NeverDerefAliases, req.DerefAliases)
			assert.Equal(t, []string{"uid"}, req.Attributes)
			return directoryResult(dn, "UID", "Alice"), nil
		},
		groups: func(req *ldap.SearchRequest, size uint32) (*ldap.SearchResult, error) {
			assert.True(t, user.closed, "close the user connection before resolving groups")
			assert.Equal(t, uint32(500), size)
			assert.Equal(t, "(&(objectClass=group)(member=CN=Alice\\2a\\28admin\\29,DC=example,DC=com))", req.Filter)
			assert.Equal(t, cfg.GroupBaseDN, req.BaseDN)
			assert.Equal(t, ldap.ScopeWholeSubtree, req.Scope)
			assert.Equal(t, ldap.NeverDerefAliases, req.DerefAliases)
			assert.Equal(t, []string{"cn"}, req.Attributes)
			return &ldap.SearchResult{Entries: []*ldap.Entry{
				ldap.NewEntry("", map[string][]string{"cn": {"Developers"}, "ignored": {"system:masters"}}),
				ldap.NewEntry("", map[string][]string{"cn": {"Developers"}}),
				ldap.NewEntry("", map[string][]string{"cn": {"developers"}}),
			}}, nil
		},
	}
	d := mockDirectory(t, cfg, search, user)
	canonical, groups, err := d.verify(t.Context(), "alice*)(uid=*)\x00", " secret\r\n\x00 ")
	require.NoError(t, err)
	assert.Equal(t, "Alice", canonical)
	assert.Equal(t, []string{"Developers", "developers"}, groups)
	assert.Equal(t, 1, search.binds)
	assert.Equal(t, 1, user.binds)
	assert.True(t, search.closed)
	assert.True(t, user.closed)
}

func TestLDAPDirectoryVerifyFailures(t *testing.T) {
	diagnostic := errors.New("secret password and account diagnostics")
	cases := []struct {
		name     string
		change   func(*directoryClient, *directoryClient)
		want     error
		userBind bool
	}{
		{name: "search account rejection", change: func(s, _ *directoryClient) {
			s.bind = func(string, string) error { return ldap.NewError(ldap.LDAPResultInvalidCredentials, diagnostic) }
		}, want: ErrDirectoryUnavailable},
		{name: "no match", change: func(s, _ *directoryClient) { s.result = &ldap.SearchResult{} }, want: ErrBadCredentials},
		{name: "ambiguous match", change: func(s, _ *directoryClient) { s.result.Entries = append(s.result.Entries, s.result.Entries[0]) }, want: ErrBadCredentials},
		{name: "partial user search", change: func(s, _ *directoryClient) {
			s.searchError = ldap.NewError(ldap.LDAPResultSizeLimitExceeded, diagnostic)
		}, want: ErrDirectoryUnavailable},
		{name: "user referral", change: func(s, _ *directoryClient) { s.result.Referrals = []string{"ldap://other.example"} }, want: ErrDirectoryUnavailable},
		{name: "nil user result", change: func(s, _ *directoryClient) { s.result = nil }, want: ErrDirectoryUnavailable},
		{name: "missing DN", change: func(s, _ *directoryClient) { s.result.Entries[0].DN = "" }, want: ErrDirectoryUnavailable},
		{name: "malformed DN", change: func(s, _ *directoryClient) { s.result.Entries[0].DN = "invalid" }, want: ErrDirectoryUnavailable},
		{name: "rejected password", change: func(_, u *directoryClient) {
			u.bind = func(string, string) error { return ldap.NewError(ldap.LDAPResultInvalidCredentials, diagnostic) }
		}, want: ErrBadCredentials, userBind: true},
		{name: "user bind referral", change: func(_, u *directoryClient) {
			u.bind = func(string, string) error { return ldap.NewError(ldap.LDAPResultReferral, diagnostic) }
		}, want: ErrDirectoryUnavailable, userBind: true},
		{name: "user bind network failure", change: func(_, u *directoryClient) { u.bind = func(string, string) error { return diagnostic } }, want: ErrDirectoryUnavailable, userBind: true},
		{name: "partial groups", change: func(s, _ *directoryClient) {
			s.groupError = ldap.NewError(ldap.LDAPResultTimeLimitExceeded, diagnostic)
		}, want: ErrDirectoryUnavailable, userBind: true},
		{name: "group referral", change: func(s, _ *directoryClient) { s.groupResult.Referrals = []string{"ldap://other.example"} }, want: ErrDirectoryUnavailable, userBind: true},
		{name: "non-success group result", change: func(s, _ *directoryClient) { s.groupResult.ResultCode = ldap.LDAPResultAdminLimitExceeded }, want: ErrDirectoryUnavailable, userBind: true},
		{name: "nil group result", change: func(s, _ *directoryClient) { s.groupResult = nil }, want: ErrDirectoryUnavailable, userBind: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			search, user := directoryClients()
			tc.change(search, user)
			d := mockDirectory(t, directoryConfig(), search, user)
			name, groups, err := d.verify(t.Context(), "alice", "password")
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, tc.want.Error(), err.Error())
			assert.Empty(t, name)
			assert.Nil(t, groups)
			assert.True(t, search.closed)
			if tc.userBind {
				assert.Equal(t, 1, user.binds)
				assert.True(t, user.closed)
			} else {
				assert.Zero(t, user.binds)
			}
			if user.binds == 0 || user.bind != nil {
				assert.Zero(t, search.groupCalls)
			}
		})
	}
}

func TestLDAPDirectoryVerifyIdentities(t *testing.T) {
	for _, attribute := range []string{"uid", "cn"} {
		for _, values := range [][]string{nil, {"a", "b"}, {""}, {" \t"}, {" Alice"}, {"Alice "}, {"system:masters"}, {"a\x00b"}, {"\xff"}} {
			t.Run(fmt.Sprintf("%s/%q", attribute, values), func(t *testing.T) {
				search, user := directoryClients()
				entry := search.result.Entries[0]
				if attribute == "cn" {
					entry = search.groupResult.Entries[0]
				}
				entry.Attributes = []*ldap.EntryAttribute{{Name: attribute, Values: values}}
				d := mockDirectory(t, directoryConfig(), search, user)
				_, _, err := d.verify(t.Context(), "alice", "password")
				require.ErrorIs(t, err, ErrDirectoryUnavailable)
			})
		}
	}
}

func TestLDAPDirectoryVerifyEmptyGroups(t *testing.T) {
	search, user := directoryClients()
	search.groupResult = &ldap.SearchResult{}
	d := mockDirectory(t, directoryConfig(), search, user)
	name, groups, err := d.verify(t.Context(), "alice", "password")
	require.NoError(t, err)
	assert.Equal(t, "Alice", name)
	assert.Equal(t, []string{}, groups)
}

func TestLDAPDirectoryVerifyBeforeConnecting(t *testing.T) {
	for _, input := range [][2]string{{"", "password"}, {"alice", ""}} {
		d := &ldapDirectory{cfg: directoryConfig()}
		_, _, err := d.verify(t.Context(), input[0], input[1])
		require.ErrorIs(t, err, ErrBadCredentials)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d := &ldapDirectory{cfg: directoryConfig()}
	_, _, err := d.verify(ctx, "alice", "password")
	require.ErrorIs(t, err, ErrDirectoryUnavailable)
}

func TestLDAPDirectoryVerifyContext(t *testing.T) {
	type loginKey struct{}
	search, user := directoryClients()
	d := mockDirectory(t, directoryConfig(), search, user)
	connect := d.connect
	d.connect = func(ctx context.Context) (ldapClient, func(), error) {
		assert.Equal(t, "login-request", ctx.Value(loginKey{}), "verify must propagate the caller context to each connection")
		return connect(ctx)
	}
	ctx := context.WithValue(t.Context(), loginKey{}, "login-request")
	_, _, err := d.verify(ctx, "alice", "password")
	require.NoError(t, err)
}

type directoryClient struct {
	bind                    func(string, string) error
	search                  func(*ldap.SearchRequest) (*ldap.SearchResult, error)
	groups                  func(*ldap.SearchRequest, uint32) (*ldap.SearchResult, error)
	result, groupResult     *ldap.SearchResult
	searchError, groupError error
	binds, groupCalls       int
	closed                  bool
}

func (c *directoryClient) Bind(dn, password string) error {
	c.binds++
	if c.bind != nil {
		return c.bind(dn, password)
	}
	return nil
}

func (c *directoryClient) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	if c.search != nil {
		return c.search(req)
	}
	return c.result, c.searchError
}

func (c *directoryClient) SearchWithPaging(req *ldap.SearchRequest, size uint32) (*ldap.SearchResult, error) {
	c.groupCalls++
	if c.groups != nil {
		return c.groups(req, size)
	}
	return c.groupResult, c.groupError
}

func directoryConfig() config.LDAPConfig {
	return config.LDAPConfig{
		URL: "ldaps://directory.example", BindDN: "cn=search,dc=example", BindPassword: "search-secret",
		UserBaseDN: "ou=users,dc=example", UserFilter: "(uid={username})", UsernameAttribute: "uid",
		GroupBaseDN: "ou=groups,dc=example", GroupFilter: "(&(objectClass=group)(member={userDN}))", GroupNameAttribute: "cn",
		TimeoutSeconds: 2,
	}
}

func directoryResult(dn, attribute, value string) *ldap.SearchResult {
	return &ldap.SearchResult{Entries: []*ldap.Entry{ldap.NewEntry(dn, map[string][]string{attribute: {value}})}}
}

func directoryClients() (*directoryClient, *directoryClient) {
	return &directoryClient{
		result:      directoryResult("uid=alice,dc=example", "uid", "Alice"),
		groupResult: directoryResult("cn=developers,dc=example", "cn", "Developers"),
	}, &directoryClient{}
}

func mockDirectory(t *testing.T, cfg config.LDAPConfig, clients ...*directoryClient) *ldapDirectory {
	t.Helper()
	d, err := newLDAPDirectory(cfg)
	require.NoError(t, err)
	index := 0
	d.connect = func(context.Context) (ldapClient, func(), error) {
		require.Less(t, index, len(clients), "unexpected connection or credential retry")
		client := clients[index]
		index++
		return client, func() { client.closed = true }, nil
	}
	return d
}

func TestLDAPDirectoryVerifyTLSAndPaging(t *testing.T) {
	certificate := directoryCertificate(t, "127.0.0.1")
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			cfg := directoryConfig()
			var binds atomic.Int32
			t.Cleanup(func() { assert.Equal(t, int32(2), binds.Load()) })
			address := serveLDAP(t, func(raw net.Conn) error {
				conn, err := secureLDAP(raw, certificate)
				if err != nil {
					return err
				}
				dn, password, err := acceptLDAPBind(conn)
				if err != nil {
					return err
				}
				binds.Add(1)
				if dn != cfg.BindDN {
					assert.Equal(t, "uid=alice,dc=example", dn)
					assert.Equal(t, " secret ", password)
					return expectLDAPClose(conn)
				}
				assert.Equal(t, cfg.BindPassword, password)
				if err := replyLDAPUser(conn); err != nil {
					return err
				}
				return replyLDAPGroupPages(conn, partial)
			})
			cfg.URL, cfg.CACert = "ldaps://"+address, directoryCA(certificate)
			d, err := newLDAPDirectory(cfg)
			require.NoError(t, err)
			name, groups, err := d.verify(t.Context(), "alice", " secret ")
			if partial {
				require.ErrorIs(t, err, ErrDirectoryUnavailable)
				assert.Empty(t, name)
				assert.Nil(t, groups)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "Alice", name)
				assert.Equal(t, []string{"Team0", "Team1"}, groups)
			}
		})
	}
}

func TestLDAPDirectoryVerifyConcurrentTransport(t *testing.T) {
	certificate := directoryCertificate(t, "127.0.0.1")
	cfg := directoryConfig()
	var searchBinds, userBinds atomic.Int32
	ready := make(chan struct{})
	t.Cleanup(func() { assert.Equal(t, int32(2), userBinds.Load()) })
	address := serveLDAP(t, func(raw net.Conn) error {
		conn, err := secureLDAP(raw, certificate)
		if err != nil {
			return err
		}
		dn, password, err := acceptLDAPBind(conn)
		if err != nil {
			return err
		}
		if dn != cfg.BindDN {
			passwords := map[string]string{
				"uid=alice,dc=example": "password-alice",
				"uid=bob,dc=example":   "password-bob",
			}
			want, ok := passwords[dn]
			if !ok || password != want {
				return errors.New("credentials crossed concurrent logins")
			}
			userBinds.Add(1)
			return expectLDAPClose(conn)
		}
		assert.Equal(t, cfg.BindPassword, password)
		// Both search connections must be bound before either login proceeds.
		if searchBinds.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			return errors.New("concurrent login did not reach search bind")
		}
		request, err := readLDAPRequest(conn, ldap.ApplicationSearchRequest)
		if err != nil {
			return err
		}
		if len(request.Children[1].Children) < 7 {
			return errors.New("missing user search filter")
		}
		filter, err := ldap.DecompileFilter(request.Children[1].Children[6])
		if err != nil {
			return err
		}
		var name string
		switch filter {
		case "(uid=alice)":
			name = "alice"
		case "(uid=bob)":
			name = "bob"
		default:
			return fmt.Errorf("unexpected user search filter %q", filter)
		}
		if err := writeLDAPEntry(conn, request, "uid="+name+",dc=example", "uid", name); err != nil {
			return err
		}
		if err := writeLDAPResult(conn, request, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess, nil); err != nil {
			return err
		}
		request, err = readLDAPRequest(conn, ldap.ApplicationSearchRequest)
		if err != nil {
			return err
		}
		if err := writeLDAPResult(conn, request, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess, nil); err != nil {
			return err
		}
		return expectLDAPClose(conn)
	})
	cfg.URL, cfg.CACert = "ldaps://"+address, directoryCA(certificate)
	d, err := newLDAPDirectory(cfg)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for _, name := range []string{"alice", "bob"} {
		wg.Go(func() {
			canonical, groups, err := d.verify(t.Context(), name, "password-"+name)
			if assert.NoError(t, err, "login %s", name) {
				assert.Equal(t, name, canonical)
				assert.Equal(t, []string{}, groups)
			}
		})
	}
	wg.Wait()
}

func TestLDAPDirectoryVerifyRejectsCertificates(t *testing.T) {
	certificate := directoryCertificate(t, "127.0.0.1")
	untrusted := directoryCertificate(t, "127.0.0.1")
	wrongHost := directoryCertificate(t, "wrong.example")
	cases := []struct {
		name        string
		connection  int32
		certificate tls.Certificate
	}{
		{name: "search/untrusted", connection: 1, certificate: untrusted},
		{name: "user/untrusted", connection: 2, certificate: untrusted},
		{name: "search/hostname", connection: 1, certificate: wrongHost},
		{name: "user/hostname", connection: 2, certificate: wrongHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var connections, binds atomic.Int32
			t.Cleanup(func() { assert.Equal(t, tc.connection-1, binds.Load()) })
			address := serveLDAP(t, func(raw net.Conn) error {
				index := connections.Add(1)
				selected := certificate
				if index == tc.connection {
					selected = tc.certificate
				}
				conn, err := secureLDAP(raw, selected)
				if err != nil {
					if index == tc.connection {
						return nil // Expected certificate alert from the client.
					}
					return err
				}
				if _, _, err := acceptLDAPBind(conn); err != nil {
					return err
				}
				binds.Add(1)
				if index == 1 {
					if err := replyLDAPUser(conn); err != nil {
						return err
					}
				}
				return expectLDAPClose(conn)
			})
			cfg := directoryConfig()
			cfg.URL = "ldaps://" + address
			// Trust the wrong-host certificate so only hostname verification rejects it.
			cfg.CACert = directoryCA(certificate, wrongHost)
			d, err := newLDAPDirectory(cfg)
			require.NoError(t, err)
			_, _, err = d.verify(t.Context(), "alice", "password")
			require.ErrorIs(t, err, ErrDirectoryUnavailable)
		})
	}
}

func TestLDAPDirectoryVerifyCancellation(t *testing.T) {
	certificate := directoryCertificate(t, "127.0.0.1")
	cases := []struct {
		stage    string
		deadline bool
	}{
		{stage: "TLS"},
		{stage: "search bind"},
		{stage: "lookup"},
		{stage: "user bind"},
		{stage: "groups"},
		{stage: "TLS", deadline: true},
		{stage: "search bind", deadline: true},
		{stage: "lookup", deadline: true},
		{stage: "user bind", deadline: true},
		{stage: "groups", deadline: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/deadline=%t", tc.stage, tc.deadline), func(t *testing.T) {
			entered := make(chan struct{})
			address := serveLDAP(t, stalledLDAPLogin(certificate, tc.stage, entered))
			cfg := directoryConfig()
			cfg.URL, cfg.CACert = "ldaps://"+address, directoryCA(certificate)
			// Keep deadline expiry outside the assertion window for cancellation cases.
			cfg.TimeoutSeconds = 10
			if tc.deadline {
				cfg.TimeoutSeconds = 1
			}
			d, err := newLDAPDirectory(cfg)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, _, err := d.verify(ctx, "alice", "password"); result <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("login did not reach blocked operation")
			}
			if !tc.deadline {
				cancel()
			}
			select {
			case err := <-result:
				require.ErrorIs(t, err, ErrDirectoryUnavailable)
			case <-time.After(3 * time.Second):
				t.Fatal("login did not stop")
			}
		})
	}
}

func TestNewLDAPDirectory(t *testing.T) {
	for _, cfg := range []config.LDAPConfig{
		{URL: "%"},
		{URL: "https://example.com"},
		{URL: "ldaps://"},
		{URL: "ldap://example.com:389"},
		{URL: "ldaps://example.com", CACert: "invalid"},
	} {
		_, err := newLDAPDirectory(cfg)
		require.ErrorIs(t, err, ErrDirectoryUnavailable, "URL %q", cfg.URL)
	}
}

func stalledLDAPLogin(certificate tls.Certificate, stage string, entered chan<- struct{}) func(net.Conn) error {
	var connections atomic.Int32
	return func(raw net.Conn) error {
		index := connections.Add(1)
		if stage == "TLS" {
			// Wait for the client to begin TLS setup before allowing cancellation.
			var header [5]byte
			if _, err := io.ReadFull(raw, header[:]); err != nil {
				return err
			}
			close(entered)
			_, err := io.Copy(io.Discard, raw)
			return err
		}
		conn, err := secureLDAP(raw, certificate)
		if err != nil {
			return err
		}
		if (stage == "search bind" && index == 1) || (stage == "user bind" && index == 2) {
			return stallLDAPRequest(conn, entered)
		}
		if _, _, err := acceptLDAPBind(conn); err != nil {
			return err
		}
		if index == 2 {
			return expectLDAPClose(conn)
		}
		if stage == "lookup" {
			return stallLDAPRequest(conn, entered)
		}
		if err := replyLDAPUser(conn); err != nil {
			return err
		}
		if stage == "user bind" {
			return expectLDAPClose(conn)
		}
		return stallLDAPRequest(conn, entered)
	}
}

func stallLDAPRequest(conn net.Conn, entered chan<- struct{}) error {
	if _, err := ber.ReadPacket(conn); err != nil {
		return err
	}
	close(entered)
	return expectLDAPClose(conn)
}

func acceptLDAPBind(conn net.Conn) (string, string, error) {
	packet, err := readLDAPRequest(conn, ldap.ApplicationBindRequest)
	if err != nil {
		return "", "", err
	}
	bind := packet.Children[1]
	if len(bind.Children) != 3 {
		return "", "", errors.New("malformed bind request")
	}
	err = writeLDAPResult(conn, packet, ldap.ApplicationBindResponse, ldap.LDAPResultSuccess, nil)
	return bind.Children[1].Data.String(), bind.Children[2].Data.String(), err
}

func replyLDAPUser(conn net.Conn) error {
	packet, err := readLDAPRequest(conn, ldap.ApplicationSearchRequest)
	if err != nil {
		return err
	}
	if err := writeLDAPEntry(conn, packet, "uid=alice,dc=example", "uid", "Alice"); err != nil {
		return err
	}
	return writeLDAPResult(conn, packet, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess, nil)
}

func replyLDAPGroupPages(conn net.Conn, partial bool) error {
	for page := range 2 {
		packet, err := readLDAPRequest(conn, ldap.ApplicationSearchRequest)
		if err != nil {
			return err
		}
		if err := writeLDAPEntry(conn, packet, "cn=team,dc=example", "cn", fmt.Sprintf("Team%d", page)); err != nil {
			return err
		}
		response := ldap.NewControlPaging(500)
		code := uint16(ldap.LDAPResultSuccess)
		if page == 0 {
			response.SetCookie([]byte("next"))
		} else if partial {
			code = ldap.LDAPResultSizeLimitExceeded
		}
		if err := writeLDAPResult(conn, packet, ldap.ApplicationSearchResultDone, code, response); err != nil {
			return err
		}
	}
	return expectLDAPClose(conn)
}

func readLDAPRequest(conn net.Conn, operation ber.Tag) (*ber.Packet, error) {
	packet, err := ber.ReadPacket(conn)
	if err != nil {
		return nil, err
	}
	if len(packet.Children) < 2 || packet.Children[1].Tag != operation {
		return nil, fmt.Errorf("expected LDAP operation %d", operation)
	}
	return packet, nil
}

func directoryCertificate(t *testing.T, hostname string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{hostname}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func directoryCA(certificates ...tls.Certificate) string {
	var bundle bytes.Buffer
	for _, certificate := range certificates {
		_ = pem.Encode(&bundle, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	}
	return base64.StdEncoding.EncodeToString(bundle.Bytes())
}

func serveLDAP(t *testing.T, handle func(net.Conn) error) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); !assert.NoError(t, err) {
					return
				}
				assert.NoError(t, handle(conn))
			})
		}
	})
	t.Cleanup(func() { _ = listener.Close(); wg.Wait() })
	return listener.Addr().String()
}

func secureLDAP(raw net.Conn, certificate tls.Certificate) (net.Conn, error) {
	conn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err := conn.Handshake(); err != nil {
		return nil, err
	}
	return conn, nil
}

func expectLDAPClose(conn net.Conn) error {
	var data [1]byte
	_, err := conn.Read(data[:])
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected operation after completion or failure")
	}
	return err
}

func writeLDAPEntry(conn net.Conn, request *ber.Packet, dn, attribute, value string) error {
	entry := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationSearchResultEntry, nil, "")
	entry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, dn, ""))
	attrs := ber.NewSequence("")
	attr := ber.NewSequence("")
	attr.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, attribute, ""))
	values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "")
	values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, value, ""))
	attr.AppendChild(values)
	attrs.AppendChild(attr)
	entry.AppendChild(attrs)
	response := ber.NewSequence("")
	response.AppendChild(request.Children[0])
	response.AppendChild(entry)
	_, err := conn.Write(response.Bytes())
	return err
}

func writeLDAPResult(conn net.Conn, request *ber.Packet, tag ber.Tag, code uint16, control ldap.Control) error {
	response := ber.NewSequence("")
	response.AppendChild(request.Children[0])
	result := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tag, nil, "")
	result.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, ""))
	result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	response.AppendChild(result)
	if control != nil {
		controls := ber.Encode(ber.ClassContext, ber.TypeConstructed, 0, nil, "")
		controls.AppendChild(control.Encode())
		response.AppendChild(controls)
	}
	_, err := conn.Write(response.Bytes())
	return err
}

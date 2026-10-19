package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-cmp/cmp"
)

func TestNewLDAPTokens(t *testing.T) {
	cfg, public := tokenConfig(t, "active")
	_, otherPublic := tokenConfig(t, "other")
	for _, tc := range []struct {
		name    string
		change  func(*config.LDAPConfig)
		wantErr bool
	}{
		{name: "derived active key", change: func(*config.LDAPConfig) {}},
		{name: "matching active key", change: func(c *config.LDAPConfig) { c.VerificationKeys = map[string]string{"active": public} }},
		{name: "invalid private key", change: func(c *config.LDAPConfig) { c.SigningPrivateKey = "private secret" }, wantErr: true},
		{name: "invalid public key", change: func(c *config.LDAPConfig) { c.VerificationKeys = map[string]string{"other": "invalid"} }, wantErr: true},
		{name: "conflicting active key", change: func(c *config.LDAPConfig) { c.VerificationKeys = map[string]string{"active": otherPublic} }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := cfg
			tc.change(&input)
			_, err := newLDAPTokens(input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("newLDAPTokens() error = %v, want error presence %t", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "private secret") {
				t.Fatal("newLDAPTokens() error exposed private material")
			}
		})
	}
}

func TestLDAPTokensIssue(t *testing.T) {
	cfg, _ := tokenConfig(t, "active")
	tokens := newTokenService(t, cfg)
	for _, groups := range [][]string{nil, {}, {"Developers", "developers"}} {
		pair, err := tokens.issue("Alice", groups)
		if err != nil {
			t.Fatalf("issue(Alice, %v) error = %v", groups, err)
		}
		claims := jwt.MapClaims{}
		parsed, err := jwt.ParseWithClaims(pair.AccessToken, claims, tokens.verificationKey, jwt.WithTimeFunc(tokens.now))
		if err != nil {
			t.Fatalf("parse issued token: %v", err)
		}
		wantGroups := make([]any, len(groups))
		for i, group := range groups {
			wantGroups[i] = group
		}
		want := jwt.MapClaims{
			"iss": cfg.Issuer, "aud": cfg.Audience, "sub": "Alice", "groups": wantGroups,
			"iat": float64(tokens.now().Unix()), "exp": float64(tokens.now().Unix() + 900), "token_use": "access",
		}
		if diff := cmp.Diff(want, claims); diff != "" {
			t.Errorf("issued claims mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "active"}, parsed.Header); diff != "" {
			t.Errorf("issued headers mismatch (-want +got):\n%s", diff)
		}
		if !pair.ExpiresAt.Equal(time.Unix(tokens.now().Unix()+900, 0)) || pair.RefreshToken != "" {
			t.Errorf("issue() expiry = %v, refresh token present = %t; want matching exp and no refresh", pair.ExpiresAt, pair.RefreshToken != "")
		}
	}
	for _, identity := range []string{"", " ", " Alice", "Alice ", "system:admin", "Ali\nce", "\xff"} {
		if pair, err := tokens.issue(identity, nil); err == nil || pair.AccessToken != "" {
			t.Errorf("issue(%q) accepted invalid username", identity)
		}
		if pair, err := tokens.issue("Alice", []string{identity}); err == nil || pair.AccessToken != "" {
			t.Errorf("issue(Alice, [%q]) accepted invalid group", identity)
		}
	}

	t.Run("token size limit", func(t *testing.T) {
		pair, err := tokens.issue("Alice", []string{"g"})
		if err != nil {
			t.Fatal(err)
		}
		payload := strings.Split(pair.AccessToken, ".")[1]
		decoded, err := base64.RawURLEncoding.DecodeString(payload)
		if err != nil {
			t.Fatal(err)
		}
		const limit = 8 * 1024
		// Fill the payload budget after accounting for the header and signature.
		overhead := len(pair.AccessToken) - len(payload)
		groupBytes := base64.RawURLEncoding.DecodedLen(limit-overhead) - len(decoded) + 1
		groups := make([]string, 500)
		for i := range groups {
			groups[i] = fmt.Sprintf("group-%026d", i)
		}
		for _, tc := range []struct {
			name    string
			groups  []string
			wantErr bool
		}{
			{name: "at limit", groups: []string{strings.Repeat("g", groupBytes)}},
			{name: "above limit", groups: []string{strings.Repeat("g", groupBytes+1)}, wantErr: true},
			{name: "many groups", groups: groups, wantErr: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pair, err := tokens.issue("Alice", tc.groups)
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), "token exceeds size limit") {
						t.Fatalf("issue() error = %v, want token size error", err)
					}
					if diff := cmp.Diff(TokenPair{}, pair); diff != "" {
						t.Errorf("rejected issuance returned a token pair (-want +got):\n%s", diff)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(pair.AccessToken) != limit {
					t.Fatalf("issued token size = %d, want %d", len(pair.AccessToken), limit)
				}
				credential, err := tokens.verify(pair.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.groups, credential.ImpersonateGroups); diff != "" {
					t.Errorf("issued groups mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})
}

func TestLDAPTokensVerify(t *testing.T) {
	cfg, _ := tokenConfig(t, "active")
	tokens := newTokenService(t, cfg)
	now := tokens.now().Truncate(time.Second)
	tokens.now = func() time.Time { return now }
	base := jwt.MapClaims{
		"iss": cfg.Issuer, "aud": cfg.Audience, "sub": "Alice", "groups": []string{"Developers"},
		"iat": tokens.now().Unix(), "exp": tokens.now().Unix() + 900, "token_use": "access",
	}
	for _, tc := range []struct {
		name  string
		claim string
		value any
		want  error
	}{
		{name: "valid"},
		{name: "empty groups", claim: "groups", value: []string{}},
		{name: "audience list", claim: "aud", value: []string{"another", cfg.Audience}},
		{name: "expired", claim: "exp", value: tokens.now().Unix() - 1, want: ErrTokenExpired},
		{name: "expiry boundary", claim: "exp", value: tokens.now().Unix(), want: ErrTokenExpired},
		{name: "missing expiry", claim: "exp", want: ErrUnauthenticated},
		{name: "string expiry", claim: "exp", value: "9999999999", want: ErrUnauthenticated},
		{name: "zero expiry", claim: "exp", value: 0, want: ErrUnauthenticated},
		{name: "missing issued at", claim: "iat", want: ErrUnauthenticated},
		{name: "string issued at", claim: "iat", value: "0", want: ErrUnauthenticated},
		{name: "future issued at", claim: "iat", value: tokens.now().Unix() + 1, want: ErrUnauthenticated},
		{name: "future not before", claim: "nbf", value: tokens.now().Unix() + 1, want: ErrUnauthenticated},
		{name: "invalid not before", claim: "nbf", value: "tomorrow", want: ErrUnauthenticated},
		{name: "wrong issuer", claim: "iss", value: "other", want: ErrUnauthenticated},
		{name: "missing issuer", claim: "iss", want: ErrUnauthenticated},
		{name: "wrong audience", claim: "aud", value: "other", want: ErrUnauthenticated},
		{name: "missing audience", claim: "aud", want: ErrUnauthenticated},
		{name: "wrong purpose", claim: "token_use", value: "refresh", want: ErrUnauthenticated},
		{name: "missing purpose", claim: "token_use", want: ErrUnauthenticated},
		{name: "missing subject", claim: "sub", want: ErrUnauthenticated},
		{name: "numeric subject", claim: "sub", value: 42, want: ErrUnauthenticated},
		{name: "empty subject", claim: "sub", value: "", want: ErrUnauthenticated},
		{name: "blank subject", claim: "sub", value: "\t ", want: ErrUnauthenticated},
		{name: "surrounding subject whitespace", claim: "sub", value: " Alice ", want: ErrUnauthenticated},
		{name: "reserved subject", claim: "sub", value: "system:admin", want: ErrUnauthenticated},
		{name: "control in subject", claim: "sub", value: "Al\nice", want: ErrUnauthenticated},
		{name: "missing groups", claim: "groups", want: ErrUnauthenticated},
		{name: "null groups", claim: "groups", value: (*string)(nil), want: ErrUnauthenticated},
		{name: "string groups", claim: "groups", value: "Developers", want: ErrUnauthenticated},
		{name: "mixed groups", claim: "groups", value: []any{"Developers", 42}, want: ErrUnauthenticated},
		{name: "empty group", claim: "groups", value: []string{""}, want: ErrUnauthenticated},
		{name: "blank group", claim: "groups", value: []string{" "}, want: ErrUnauthenticated},
		{name: "surrounding group whitespace", claim: "groups", value: []string{" Developers"}, want: ErrUnauthenticated},
		{name: "reserved group", claim: "groups", value: []string{"system:masters"}, want: ErrUnauthenticated},
		{name: "control in group", claim: "groups", value: []string{"Dev\x00ops"}, want: ErrUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := maps.Clone(base)
			if tc.claim != "" {
				if tc.value == nil {
					delete(claims, tc.claim)
				} else {
					claims[tc.claim] = tc.value
				}
			}
			raw := signToken(t, tokens, claims, nil)
			credential, err := tokens.verify(raw)
			if !errors.Is(err, tc.want) {
				t.Fatalf("verify() error = %v, want %v", err, tc.want)
			}
			if err != nil {
				if diff := cmp.Diff(ClusterCredential{}, credential); diff != "" {
					t.Errorf("rejected token returned credentials (-want +got):\n%s", diff)
				}
				return
			}
			groups, ok := claims["groups"].([]string)
			if !ok {
				t.Fatal("valid fixture groups must be strings")
			}
			want := ClusterCredential{ImpersonateUser: "Alice", ImpersonateGroups: groups}
			if diff := cmp.Diff(want, credential); diff != "" {
				t.Errorf("verify() credentials mismatch (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("invalid headers and signatures", func(t *testing.T) {
		otherCfg, _ := tokenConfig(t, "active")
		other := newTokenService(t, otherCfg)
		pair, err := tokens.issue("Alice", nil)
		if err != nil {
			t.Fatal(err)
		}
		tamperedClaims := maps.Clone(base)
		tamperedClaims["sub"] = "Mallory"
		tampered := jwt.NewWithClaims(jwt.SigningMethodEdDSA, tamperedClaims)
		tampered.Header["kid"] = cfg.SigningKeyID
		unsigned, err := tampered.SigningString()
		if err != nil {
			t.Fatal(err)
		}
		signature := strings.Split(pair.AccessToken, ".")[2]
		for name, raw := range map[string]string{
			"missing":           "",
			"malformed":         "not.a.jwt",
			"tampered":          unsigned + "." + signature,
			"wrong signing key": signToken(t, other, base, nil),
			"missing kid":       signToken(t, tokens, base, map[string]any{"kid": nil}),
			"empty kid":         signToken(t, tokens, base, map[string]any{"kid": ""}),
			"numeric kid":       signToken(t, tokens, base, map[string]any{"kid": 42}),
			"unknown kid":       signToken(t, tokens, base, map[string]any{"kid": "unknown", "jku": "https://invalid.example/keys"}),
			"wrong type":        signToken(t, tokens, base, map[string]any{"typ": "refresh"}),
			"missing type":      signToken(t, tokens, base, map[string]any{"typ": nil}),
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := tokens.verify(raw); !errors.Is(err, ErrUnauthenticated) {
					t.Errorf("verify() = %v, want ErrUnauthenticated", err)
				}
			})
		}
		for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodNone} {
			token := jwt.NewWithClaims(method, base)
			token.Header["kid"] = cfg.SigningKeyID
			var key any = []byte("secret")
			if method == jwt.SigningMethodNone {
				key = jwt.UnsafeAllowNoneSignatureType
			}
			raw, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tokens.verify(raw); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("verify(%s) = %v, want ErrUnauthenticated", method.Alg(), err)
			}
		}
	})
	t.Run("expiry does not mask invalid trust or purpose", func(t *testing.T) {
		for claim, value := range map[string]any{"iss": "other", "aud": "other", "iat": tokens.now().Unix() + 1, "nbf": tokens.now().Unix() + 1, "token_use": "refresh"} {
			claims := maps.Clone(base)
			claims["exp"] = tokens.now().Unix() - 1
			claims[claim] = value
			if _, err := tokens.verify(signToken(t, tokens, claims, nil)); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("verify(expired with invalid %s) = %v, want ErrUnauthenticated", claim, err)
			}
		}
	})
}

func TestLDAPTokensVerifyRotation(t *testing.T) {
	oldCfg, oldPublic := tokenConfig(t, "old")
	newCfg, newPublic := tokenConfig(t, "new")
	oldCfg.VerificationKeys = map[string]string{"new": newPublic}
	newCfg.VerificationKeys = map[string]string{"old": oldPublic}
	oldInstance := newTokenService(t, oldCfg)
	newInstance := newTokenService(t, newCfg)
	// These instances have no directory dependency, and even unusable LDAP settings
	// cannot affect local issuance or verification.
	oldCfg.URL = "ldap://invalid.example"
	oldCfg.VerificationKeys["new"] = "changed after construction"
	for _, tc := range []struct {
		issuer   *ldapTokens
		verifier *ldapTokens
	}{
		{issuer: oldInstance, verifier: oldInstance},
		{issuer: oldInstance, verifier: newInstance},
		{issuer: newInstance, verifier: oldInstance},
	} {
		pair, err := tc.issuer.issue("Alice", []string{"Developers"})
		if err != nil {
			t.Fatal(err)
		}
		credential, err := tc.verifier.verify(pair.AccessToken)
		if err != nil {
			t.Fatalf("verify(%s on %s): %v", tc.issuer.keyID, tc.verifier.keyID, err)
		}
		want := ClusterCredential{ImpersonateUser: "Alice", ImpersonateGroups: []string{"Developers"}}
		if diff := cmp.Diff(want, credential); diff != "" {
			t.Errorf("cross-instance credential mismatch (-want +got):\n%s", diff)
		}
	}
	oldPair, err := oldInstance.issue("Alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	newCfg.VerificationKeys = nil
	retired := newTokenService(t, newCfg)
	if _, err := retired.verify(oldPair.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("verify(retired key) = %v, want ErrUnauthenticated", err)
	}
}

func tokenConfig(t *testing.T, id string) (config.LDAPConfig, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return config.LDAPConfig{
		URL: "ldap://invalid.example", Issuer: "capp", Audience: "capp-api", TokenTTLSeconds: 900,
		SigningKeyID: id, SigningPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
	}, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
}

func newTokenService(t *testing.T, cfg config.LDAPConfig) *ldapTokens {
	t.Helper()
	tokens, err := newLDAPTokens(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tokens.now = func() time.Time { return time.Date(2026, time.October, 18, 12, 0, 0, 123456789, time.UTC) }
	return tokens
}

func signToken(t *testing.T, tokens *ldapTokens, claims jwt.MapClaims, headers map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = tokens.keyID
	for name, value := range headers {
		if value == nil {
			delete(token.Header, name)
		} else {
			token.Header[name] = value
		}
	}
	raw, err := token.SignedString(tokens.private)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

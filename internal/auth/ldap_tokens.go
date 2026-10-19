package auth

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

// ldapTokens holds local signing and trust material, independent of the directory.
type ldapTokens struct {
	keyID    string
	private  ed25519.PrivateKey
	keys     map[string]ed25519.PublicKey
	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time
}

// newLDAPTokens consumes configuration already checked by config.Validate.
func newLDAPTokens(cfg config.LDAPConfig) (*ldapTokens, error) {
	parsed, err := jwt.ParseEdPrivateKeyFromPEM([]byte(cfg.SigningPrivateKey))
	private, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		return nil, errors.New("auth: invalid LDAP signing key")
	}
	active, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("auth: invalid LDAP signing key")
	}
	keys := map[string]ed25519.PublicKey{cfg.SigningKeyID: active}
	for id, value := range cfg.VerificationKeys {
		parsed, err := jwt.ParseEdPublicKeyFromPEM([]byte(value))
		public, ok := parsed.(ed25519.PublicKey)
		if err != nil || !ok {
			return nil, errors.New("auth: invalid LDAP verification key")
		}
		if active, exists := keys[id]; exists && !active.Equal(public) {
			return nil, errors.New("auth: conflicting LDAP signing key ID")
		}
		keys[id] = public
	}
	return &ldapTokens{
		keyID: cfg.SigningKeyID, private: private, keys: keys,
		issuer: cfg.Issuer, audience: cfg.Audience,
		ttl: time.Duration(cfg.TokenTTLSeconds) * time.Second, now: time.Now,
	}, nil
}

// issue is called only after successful password verification and group resolution.
func (t *ldapTokens) issue(username string, groups []string) (TokenPair, error) {
	if !validLDAPIdentity(username) {
		return TokenPair{}, errors.New("auth: invalid LDAP token identity")
	}
	for _, group := range groups {
		if !validLDAPIdentity(group) {
			return TokenPair{}, errors.New("auth: invalid LDAP token identity")
		}
	}
	if groups == nil {
		groups = []string{}
	}
	now := jwt.NewNumericDate(t.now().UTC())
	expiresAt := jwt.NewNumericDate(now.Add(t.ttl))
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": t.issuer, "aud": t.audience, "sub": username, "groups": groups,
		"iat": now, "exp": expiresAt, "token_use": "access",
	})
	token.Header["kid"] = t.keyID
	signed, err := token.SignedString(t.private)
	if err != nil {
		return TokenPair{}, errors.New("auth: LDAP token signing failed")
	}
	const maxTokenBytes = 8 * 1024
	if len(signed) > maxTokenBytes {
		return TokenPair{}, errors.New("auth: LDAP token exceeds size limit")
	}
	return TokenPair{AccessToken: signed, ExpiresAt: expiresAt.Time}, nil
}

func (t *ldapTokens) verify(raw string) (ClusterCredential, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, t.verificationKey,
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(t.issuer), jwt.WithAudience(t.audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(t.now),
	)
	if err != nil && !errors.Is(err, jwt.ErrTokenExpired) {
		return ClusterCredential{}, ErrUnauthenticated
	}
	credential, identityErr := ldapTokenCredential(claims)
	if identityErr != nil {
		return ClusterCredential{}, identityErr
	}
	if err != nil {
		// Expiry must not mask other claim failures reported alongside it.
		for _, invalid := range []error{
			jwt.ErrTokenInvalidIssuer, jwt.ErrTokenInvalidAudience,
			jwt.ErrTokenRequiredClaimMissing, jwt.ErrInvalidType,
			jwt.ErrTokenUsedBeforeIssued, jwt.ErrTokenNotValidYet,
		} {
			if errors.Is(err, invalid) {
				return ClusterCredential{}, ErrUnauthenticated
			}
		}
		return ClusterCredential{}, ErrTokenExpired
	}
	return credential, nil
}

func (t *ldapTokens) verificationKey(token *jwt.Token) (any, error) {
	id, ok := token.Header["kid"].(string)
	if !ok || id == "" || token.Method != jwt.SigningMethodEdDSA || token.Header["typ"] != "JWT" {
		return nil, ErrUnauthenticated
	}
	key, ok := t.keys[id]
	if !ok {
		return nil, ErrUnauthenticated
	}
	return key, nil
}

func ldapTokenCredential(claims jwt.MapClaims) (ClusterCredential, error) {
	// WithIssuedAt validates a supplied date but does not require one.
	// MapClaims decodes JSON numbers as float64.
	if _, ok := claims["iat"].(float64); !ok {
		return ClusterCredential{}, ErrUnauthenticated
	}
	if claims["token_use"] != "access" {
		return ClusterCredential{}, ErrUnauthenticated
	}
	username, err := claims.GetSubject()
	if err != nil || !validLDAPIdentity(username) {
		return ClusterCredential{}, ErrUnauthenticated
	}
	values, ok := claims["groups"].([]any)
	if !ok {
		return ClusterCredential{}, ErrUnauthenticated
	}
	groups := make([]string, 0, len(values))
	for _, value := range values {
		group, ok := value.(string)
		if !ok || !validLDAPIdentity(group) {
			return ClusterCredential{}, ErrUnauthenticated
		}
		groups = append(groups, group)
	}
	return ClusterCredential{ImpersonateUser: username, ImpersonateGroups: groups}, nil
}

// session.go contains the shared session store used by jwtManager and dexManager.
// It manages in-memory JWT sessions: creation, authentication, refresh, and cleanup.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// jwtClaims extends the standard JWT registered claims with capp-backend
// specific fields. Both access and refresh tokens share this struct; the
// "type" claim distinguishes them.
type jwtClaims struct {
	jwt.RegisteredClaims

	// Type is "access" or "refresh". Validated on every incoming request so
	// that a refresh token cannot be used as an access token and vice-versa.
	Type string `json:"type"`

	// Cluster is the name of the cluster the session was authenticated against.
	// Present only on access tokens; used for routing in multi-cluster setups.
	Cluster string `json:"cluster,omitempty"`
}

// sessionEntry holds the cluster token and metadata for one active session.
// It lives in the sessionStore's in-memory session store.
type sessionEntry struct {
	// clusterName is the target cluster this session is bound to.
	clusterName string

	// clusterToken is the raw Kubernetes bearer token validated at login time.
	// It is used to build the ClusterCredential on every authenticated request.
	clusterToken string

	// expiresAt is the wall-clock time after which this entry is invalid and
	// eligible for garbage collection.
	expiresAt time.Time
}

// sessionStore manages in-memory JWT sessions shared across auth manager
// implementations. Sessions are stored in a sync.Map keyed by session ID.
type sessionStore struct {
	cfg      *config.JWTConfig
	sessions sync.Map
}

// newSessionStore returns a sessionStore configured with the given JWTConfig.
func newSessionStore(cfg *config.JWTConfig) *sessionStore {
	return &sessionStore{cfg: cfg}
}

// createSession creates a new session for the given clusterName and
// clusterToken, stores it in the session map, and returns a signed TokenPair.
func (s *sessionStore) createSession(clusterName, clusterToken string) (TokenPair, error) {
	sessionID := uuid.New().String()
	refreshTTL := time.Duration(s.cfg.RefreshTTLMinutes) * time.Minute

	entry := &sessionEntry{
		clusterName:  clusterName,
		clusterToken: clusterToken,
		expiresAt:    time.Now().Add(refreshTTL),
	}
	s.sessions.Store(sessionID, entry)

	return s.issueTokenPair(sessionID, clusterName)
}

// authenticate extracts the JWT from the Authorization header, validates it,
// and retrieves the session's cluster credential.
func (s *sessionStore) authenticate(r *http.Request) (ClusterCredential, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ClusterCredential{}, ErrUnauthenticated
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return ClusterCredential{}, ErrUnauthenticated
	}

	tokenStr := strings.TrimPrefix(raw, prefix)

	claims, err := s.parseToken(tokenStr)
	if err != nil {
		return ClusterCredential{}, err
	}

	if claims.Type != "access" {
		return ClusterCredential{}, fmt.Errorf("%w: expected access token, got %q", ErrInvalidToken, claims.Type)
	}

	sessionID := claims.Subject
	val, ok := s.sessions.Load(sessionID)
	if !ok {
		return ClusterCredential{}, fmt.Errorf("%w: session not found", ErrUnauthenticated)
	}

	entry := val.(*sessionEntry)
	if time.Now().After(entry.expiresAt) {
		s.sessions.Delete(sessionID)
		return ClusterCredential{}, ErrTokenExpired
	}

	return ClusterCredential{BearerToken: entry.clusterToken}, nil
}

// refresh validates the refresh token, updates the session TTL, and issues a
// new TokenPair with fresh JWTs.
func (s *sessionStore) refresh(refreshToken string) (TokenPair, error) {
	claims, err := s.parseToken(refreshToken)
	if err != nil {
		return TokenPair{}, err
	}

	if claims.Type != "refresh" {
		return TokenPair{}, fmt.Errorf("%w: expected refresh token, got %q", ErrInvalidToken, claims.Type)
	}

	sessionID := claims.Subject
	val, ok := s.sessions.Load(sessionID)
	if !ok {
		return TokenPair{}, fmt.Errorf("%w: session not found or expired", ErrUnauthenticated)
	}

	entry := val.(*sessionEntry)
	if time.Now().After(entry.expiresAt) {
		s.sessions.Delete(sessionID)
		return TokenPair{}, ErrTokenExpired
	}

	// Extend the session's lifetime on successful refresh.
	entry.expiresAt = time.Now().Add(time.Duration(s.cfg.RefreshTTLMinutes) * time.Minute)
	s.sessions.Store(sessionID, entry)

	return s.issueTokenPair(sessionID, entry.clusterName)
}

// StartCleanup runs a background goroutine that evicts expired sessions every
// 5 minutes. It blocks until ctx is cancelled, so call it as:
//
//	go store.StartCleanup(ctx)
func (s *sessionStore) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			s.sessions.Range(func(key, value any) bool {
				entry, ok := value.(*sessionEntry)
				if ok && now.After(entry.expiresAt) {
					s.sessions.Delete(key)
				}
				return true
			})
		}
	}
}

// issueTokenPair signs and returns a new access + refresh JWT pair for the
// given session.
func (s *sessionStore) issueTokenPair(sessionID, clusterName string) (TokenPair, error) {
	now := time.Now()
	accessExpiry := now.Add(time.Duration(s.cfg.TokenTTLMinutes) * time.Minute)
	refreshExpiry := now.Add(time.Duration(s.cfg.RefreshTTLMinutes) * time.Minute)

	accessToken, err := s.signToken(jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sessionID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(accessExpiry),
		},
		Type:    "access",
		Cluster: clusterName,
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: signing access token: %w", err)
	}

	refreshToken, err := s.signToken(jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sessionID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(refreshExpiry),
		},
		Type: "refresh",
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: signing refresh token: %w", err)
	}

	return TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    accessExpiry,
	}, nil
}

// signToken creates a signed JWT string from the provided claims.
func (s *sessionStore) signToken(claims jwtClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.cfg.SecretKey))
}

// parseToken validates the JWT signature and expiry and returns its claims.
func (s *sessionStore) parseToken(tokenStr string) (*jwtClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &jwtClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, t.Header["alg"])
		}
		return []byte(s.cfg.SecretKey), nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}

	claims, ok := token.Claims.(*jwtClaims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

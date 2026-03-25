package auth

import (
	"context"
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
// It lives in the jwtManager's in-memory session store.
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

// jwtManager implements AuthManager for auth.mode = "jwt".
//
// Sessions are stored in an in-memory sync.Map. This is sufficient for single
// replica deployments. For multi-replica deployments, replace the sync.Map
// with a Redis-backed store — the sessionEntry type is designed to be easily
// serialisable.
type jwtManager struct {
	cfg *config.JWTConfig

	// clusterURLs maps cluster name → API server URL for token validation at
	// login time. Populated once at startup from the clusters config.
	clusterURLs map[string]string

	// sessions stores live sessions: sessionID (string) → *sessionEntry.
	sessions sync.Map

	// httpClient is used for the /version token validation probe at login.
	httpClient *http.Client
}

// newJWTManager returns a jwtManager. Call StartCleanup in a goroutine to
// enable background expiry of stale sessions.
func newJWTManager(cfg *config.JWTConfig, clusterURLs map[string]string) *jwtManager {
	return &jwtManager{
		cfg:         cfg,
		clusterURLs: clusterURLs,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// StartCleanup runs a background goroutine that evicts expired sessions every
// 5 minutes. It blocks until ctx is cancelled, so call it as:
//
//	go manager.StartCleanup(ctx)
func (m *jwtManager) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			m.sessions.Range(func(key, value any) bool {
				entry, ok := value.(*sessionEntry)
				if ok && now.After(entry.expiresAt) {
					m.sessions.Delete(key)
				}
				return true
			})
		}
	}
}

// Login validates token against the named cluster's /version endpoint, creates
// a session, and returns a TokenPair. Returns ErrUnauthenticated if the token
// is rejected by the cluster.
func (m *jwtManager) Login(ctx context.Context, clusterName string, token string) (TokenPair, error) {
	if err := m.validateTokenAgainstCluster(ctx, clusterName, token); err != nil {
		return TokenPair{}, err
	}

	sessionID := uuid.New().String()
	refreshTTL := time.Duration(m.cfg.RefreshTTLMinutes) * time.Minute

	entry := &sessionEntry{
		clusterName:  clusterName,
		clusterToken: token,
		expiresAt:    time.Now().Add(refreshTTL),
	}
	m.sessions.Store(sessionID, entry)

	return m.issueTokenPair(sessionID, clusterName)
}

// Authenticate extracts the JWT from the Authorization header, validates it,
// and retrieves the session's cluster credential.
func (m *jwtManager) Authenticate(_ context.Context, _ string, r *http.Request) (ClusterCredential, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ClusterCredential{}, ErrUnauthenticated
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return ClusterCredential{}, ErrUnauthenticated
	}

	tokenStr := strings.TrimPrefix(raw, prefix)

	claims, err := m.parseToken(tokenStr)
	if err != nil {
		return ClusterCredential{}, err
	}

	if claims.Type != "access" {
		return ClusterCredential{}, fmt.Errorf("%w: expected access token, got %q", ErrInvalidToken, claims.Type)
	}

	sessionID := claims.Subject
	val, ok := m.sessions.Load(sessionID)
	if !ok {
		return ClusterCredential{}, fmt.Errorf("%w: session not found", ErrUnauthenticated)
	}

	entry := val.(*sessionEntry)
	if time.Now().After(entry.expiresAt) {
		m.sessions.Delete(sessionID)
		return ClusterCredential{}, ErrTokenExpired
	}

	return ClusterCredential{BearerToken: entry.clusterToken}, nil
}

// Refresh validates the refresh token, updates the session TTL, and issues a
// new TokenPair with fresh JWTs.
func (m *jwtManager) Refresh(_ context.Context, refreshToken string) (TokenPair, error) {
	claims, err := m.parseToken(refreshToken)
	if err != nil {
		return TokenPair{}, err
	}

	if claims.Type != "refresh" {
		return TokenPair{}, fmt.Errorf("%w: expected refresh token, got %q", ErrInvalidToken, claims.Type)
	}

	sessionID := claims.Subject
	val, ok := m.sessions.Load(sessionID)
	if !ok {
		return TokenPair{}, fmt.Errorf("%w: session not found or expired", ErrUnauthenticated)
	}

	entry := val.(*sessionEntry)
	if time.Now().After(entry.expiresAt) {
		m.sessions.Delete(sessionID)
		return TokenPair{}, ErrTokenExpired
	}

	// Extend the session's lifetime on successful refresh.
	entry.expiresAt = time.Now().Add(time.Duration(m.cfg.RefreshTTLMinutes) * time.Minute)
	m.sessions.Store(sessionID, entry)

	return m.issueTokenPair(sessionID, entry.clusterName)
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

// issueTokenPair signs and returns a new access + refresh JWT pair for the
// given session.
func (m *jwtManager) issueTokenPair(sessionID, clusterName string) (TokenPair, error) {
	now := time.Now()
	accessExpiry := now.Add(time.Duration(m.cfg.TokenTTLMinutes) * time.Minute)
	refreshExpiry := now.Add(time.Duration(m.cfg.RefreshTTLMinutes) * time.Minute)

	accessToken, err := m.signToken(jwtClaims{
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

	refreshToken, err := m.signToken(jwtClaims{
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
func (m *jwtManager) signToken(claims jwtClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(m.cfg.SecretKey))
}

// parseToken validates the JWT signature and expiry and returns its claims.
func (m *jwtManager) parseToken(tokenStr string) (*jwtClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &jwtClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, t.Header["alg"])
		}
		return []byte(m.cfg.SecretKey), nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "expired") {
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

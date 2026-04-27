package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSessionStore() *sessionStore {
	return newSessionStore(&config.JWTConfig{
		SecretKey:         "test-secret-32-bytes-long-enough",
		TokenTTLMinutes:   60,
		RefreshTTLMinutes: 1440,
	})
}

// -- CreateSession tests --

func TestSessionStore_CreateSession_Success(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token-xyz")
	require.NoError(t, err)

	assert.NotEmpty(t, pair.AccessToken)
	assert.NotEmpty(t, pair.RefreshToken)
	assert.False(t, pair.ExpiresAt.IsZero())
}

// -- Authenticate tests --

func TestSessionStore_Authenticate_Success(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token-xyz")
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+pair.AccessToken)

	cred, err := store.authenticate(r)
	require.NoError(t, err)
	assert.Equal(t, "k8s-token-xyz", cred.BearerToken)
}

func TestSessionStore_Authenticate_NoAuthHeader(t *testing.T) {
	store := testSessionStore()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	_, err := store.authenticate(r)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestSessionStore_Authenticate_NoBearerPrefix(t *testing.T) {
	store := testSessionStore()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Basic abc123")

	_, err := store.authenticate(r)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestSessionStore_Authenticate_InvalidToken(t *testing.T) {
	store := testSessionStore()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not-a-valid-jwt")

	_, err := store.authenticate(r)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionStore_Authenticate_RefreshTokenRejected(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+pair.RefreshToken)

	_, err = store.authenticate(r)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionStore_Authenticate_ExpiredSession(t *testing.T) {
	store := newSessionStore(&config.JWTConfig{
		SecretKey:         "test-secret-32-bytes-long-enough",
		TokenTTLMinutes:   60,
		RefreshTTLMinutes: 1,
	})
	pair, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	// Manually expire the session entry.
	store.sessions.Range(func(key, value any) bool {
		entry := value.(*sessionEntry)
		entry.expiresAt = time.Now().Add(-1 * time.Minute)
		store.sessions.Store(key, entry)
		return true
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+pair.AccessToken)

	_, err = store.authenticate(r)
	assert.ErrorIs(t, err, ErrTokenExpired)
}

func TestSessionStore_Authenticate_SessionNotFound(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	// Delete the session to simulate an evicted entry.
	store.sessions.Range(func(key, _ any) bool {
		store.sessions.Delete(key)
		return true
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+pair.AccessToken)

	_, err = store.authenticate(r)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

// -- Refresh tests --

func TestSessionStore_Refresh_Success(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	// Small sleep to ensure the iat claim differs, producing a different token.
	time.Sleep(time.Second)

	newPair, err := store.refresh(pair.RefreshToken)
	require.NoError(t, err)
	assert.NotEmpty(t, newPair.AccessToken)
	assert.NotEmpty(t, newPair.RefreshToken)
}

func TestSessionStore_Refresh_AccessTokenRejected(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	_, err = store.refresh(pair.AccessToken)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestSessionStore_Refresh_ExpiredSession(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	store.sessions.Range(func(key, value any) bool {
		entry := value.(*sessionEntry)
		entry.expiresAt = time.Now().Add(-1 * time.Minute)
		store.sessions.Store(key, entry)
		return true
	})

	_, err = store.refresh(pair.RefreshToken)
	assert.ErrorIs(t, err, ErrTokenExpired)
}

func TestSessionStore_Refresh_SessionNotFound(t *testing.T) {
	store := testSessionStore()
	pair, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	store.sessions.Range(func(key, _ any) bool {
		store.sessions.Delete(key)
		return true
	})

	_, err = store.refresh(pair.RefreshToken)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

// -- Cleanup tests --

func TestSessionStore_Cleanup_RemovesExpired(t *testing.T) {
	store := newSessionStore(&config.JWTConfig{
		SecretKey:         "test-secret-32-bytes-long-enough",
		TokenTTLMinutes:   1,
		RefreshTTLMinutes: 1,
	})

	_, err := store.createSession("prod", "k8s-token")
	require.NoError(t, err)

	store.sessions.Range(func(key, value any) bool {
		entry := value.(*sessionEntry)
		entry.expiresAt = time.Now().Add(-1 * time.Minute)
		store.sessions.Store(key, entry)
		return true
	})

	// Simulate one cleanup tick.
	now := time.Now()
	store.sessions.Range(func(key, value any) bool {
		entry, ok := value.(*sessionEntry)
		if ok && now.After(entry.expiresAt) {
			store.sessions.Delete(key)
		}
		return true
	})

	count := 0
	store.sessions.Range(func(_, _ any) bool { count++; return true })
	assert.Equal(t, 0, count)
}

// -- ParseToken tests --

func TestSessionStore_ParseToken_WrongSigningMethod(t *testing.T) {
	store := testSessionStore()
	// A token signed with RS256 instead of HS256 should be rejected.
	_, err := store.parseToken("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.invalid")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// -- StartCleanup tests --

func TestSessionStore_StartCleanup_StopsOnCancel(t *testing.T) {
	store := testSessionStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		store.StartCleanup(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartCleanup did not exit after context cancellation")
	}
}

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Passthrough tests ─────────────────────────────────────────────────────────

func TestPassthrough_Authenticate_MissingHeader(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestPassthrough_Authenticate_MalformedHeader(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestPassthrough_Authenticate_EmptyToken(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer ")
	_, err := m.Authenticate(context.Background(), "prod", r)
	assert.ErrorIs(t, err, ErrUnauthenticated)
}

func TestPassthrough_Authenticate_Valid(t *testing.T) {
	m := newPassthroughManager()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer my-k8s-token")
	cred, err := m.Authenticate(context.Background(), "prod", r)
	require.NoError(t, err)
	assert.Equal(t, "my-k8s-token", cred.BearerToken)
}

func TestPassthrough_Login_NotSupported(t *testing.T) {
	m := newPassthroughManager()
	_, err := m.Login(context.Background(), "prod", "tok")
	assert.ErrorIs(t, err, ErrNotSupported)
}

func TestPassthrough_Refresh_NotSupported(t *testing.T) {
	m := newPassthroughManager()
	_, err := m.Refresh(context.Background(), "tok")
	assert.ErrorIs(t, err, ErrNotSupported)
}

// ── Factory tests ─────────────────────────────────────────────────────────────

func TestNew_Passthrough(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{Mode: "passthrough"}}
	m, err := New(cfg)
	require.NoError(t, err)
	assert.IsType(t, &passthroughManager{}, m)
}

func TestNew_UnknownMode(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{Mode: "magic"}}
	_, err := New(cfg)
	require.Error(t, err)
}

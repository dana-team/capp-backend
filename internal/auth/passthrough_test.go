package auth

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	authenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/cert"
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
	m, err := New(cfg, Dependencies{})
	require.NoError(t, err)
	assert.IsType(t, &passthroughManager{}, m)
}

func TestNew_UnknownMode(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{Mode: "magic"}}
	_, err := New(cfg, Dependencies{})
	require.Error(t, err)
}

func TestPassthroughManagerAuthenticate(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		code         int
		want         error
	}{
		{name: "exact identity", status: `{"authenticated":true,"user":{"username":" Alice ","groups":["Developers"," developers "]}}`},
		{name: "empty groups", status: `{"authenticated":true,"user":{"username":"Alice"}}`},
		{name: "reviewer rejection including audience mismatch", status: `{"authenticated":false,"audiences":[]}`, want: ErrUnauthenticated},
		{name: "empty username", status: `{"authenticated":true}`, want: ErrUnauthenticated},
		{name: "anonymous", status: `{"authenticated":true,"user":{"username":"system:anonymous"}}`, want: ErrUnauthenticated},
		{name: "unauthenticated group", status: `{"authenticated":true,"user":{"username":"Alice","groups":["system:unauthenticated"]}}`, want: ErrUnauthenticated},
		{name: "review error", status: `{"error":"private-review-diagnostic"}`, want: ErrIdentityUnavailable},
		{name: "malformed reply", status: `invalid`, want: ErrIdentityUnavailable},
		{name: "backend token rejected", code: http.StatusUnauthorized, want: ErrIdentityUnavailable},
		{name: "backend permission denied", code: http.StatusForbidden, want: ErrIdentityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				checkReviewRequest(t, r)
				w.Header().Set("Content-Type", runtime.ContentTypeJSON)
				if tc.code != 0 {
					w.WriteHeader(tc.code)
					return
				}
				fmt.Fprintf(w, `{"apiVersion":%q,"kind":"TokenReview","status":%s}`, authv1.SchemeGroupVersion.String(), tc.status) //nolint:errcheck
			}))
			t.Cleanup(upstream.Close)
			m := passthroughReviewer(t, upstream, true)
			for range 2 {
				credential, err := m.Authenticate(t.Context(), "target", reviewRequest())
				if !errors.Is(err, tc.want) {
					t.Fatalf("Authenticate() error = %v, want %v", err, tc.want)
				}
				if err != nil {
					if diff := cmp.Diff(ClusterCredential{}, credential); diff != "" {
						t.Errorf("failed authentication returned identity (-want +got):\n%s", diff)
					}
					continue
				}
				var status authv1.TokenReviewStatus
				if err := json.Unmarshal([]byte(tc.status), &status); err != nil {
					t.Fatal(err)
				}
				want := ClusterCredential{ImpersonateUser: status.User.Username, ImpersonateGroups: status.User.Groups,
					AuthProvider: config.AuthModePassthrough, IdentityAuthority: upstream.URL}
				if diff := cmp.Diff(want, credential); diff != "" {
					t.Errorf("Authenticate() mismatch (-want +got):\n%s", diff)
				}
				if len(credential.ImpersonateGroups) != 0 {
					credential.ImpersonateGroups[0] = "changed"
				}
			}
			if got := calls.Load(); got != 2 {
				t.Errorf("review calls = %d, want 2 (no provider cache)", got)
			}
		})
	}
}

func TestPassthroughManagerAuthenticateTrust(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(upstream.Close)
	m := passthroughReviewer(t, upstream, false)
	if _, err := m.Authenticate(t.Context(), "target", reviewRequest()); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("untrusted TLS error = %v, want ErrIdentityUnavailable", err)
	}
	for _, header := range []string{"", "Basic secret", "Bearer", "Bearer ", "Bearer  ", "Bearer one two", "Bearer one\ttwo"} {
		req := reviewRequest()
		req.Header.Set("Authorization", header)
		if _, err := m.Authenticate(t.Context(), "missing", req); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) error = %v, want ErrUnauthenticated", header, err)
		}
	}
	for _, name := range []string{"", "missing"} {
		if _, err := m.Authenticate(t.Context(), name, reviewRequest()); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) error = %v, want ErrUnauthenticated", name, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("untrusted, malformed or unknown-target request reached reviewer")
	}
}

func passthroughReviewer(t *testing.T, server *httptest.Server, trust bool) AuthManager {
	t.Helper()
	cfg := &rest.Config{Host: server.URL, BearerToken: "backend-review-secret"}
	if trust {
		cfg.CAData = pem.EncodeToMemory(&pem.Block{Type: cert.CertificateBlockType, Bytes: server.Certificate().Raw})
	}
	client, err := authenticationv1.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(&config.Config{
		Auth:          config.AuthConfig{Mode: config.AuthModePassthrough},
		Authorization: config.AuthorizationConfig{Mode: config.AuthorizationModeBackendPolicy},
		Clusters:      []config.ClusterConfig{{Name: "target"}},
	}, Dependencies{TargetReviewers: map[string]TokenReviewer{"target": {Client: client, Authority: server.URL + "?secret=hidden#hidden"}}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

package auth

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/google/go-cmp/cmp"
	authv1 "k8s.io/api/authentication/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

func TestOpenShiftManagerAuthenticate(t *testing.T) {
	t.Run("review results", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			status   int
			body     string
			response string
			want     error
		}{
			{name: "accepted exact identity", body: `{"authenticated":true,"user":{"username":" Alice ","groups":["Developers"," developers "]}}`},
			{name: "accepted without groups", body: `{"authenticated":true,"user":{"username":"Alice"}}`},
			{name: "rejected token", body: `{"authenticated":false}`, want: ErrUnauthenticated},
			{name: "rejected token with omitted false flag", body: `{}`, want: ErrUnauthenticated},
			{name: "wrong audience", body: `{"authenticated":false,"audiences":[]}`, want: ErrUnauthenticated},
			{name: "empty username", body: `{"authenticated":true,"user":{"groups":["Developers"]}}`, want: ErrUnauthenticated},
			{name: "anonymous user", body: `{"authenticated":true,"user":{"username":"system:anonymous"}}`, want: ErrUnauthenticated},
			{name: "unauthenticated group", body: `{"authenticated":true,"user":{"username":"Alice","groups":["Developers","system:unauthenticated"]}}`, want: ErrUnauthenticated},
			{name: "review error with accepted flag", body: `{"authenticated":true,"error":"private-review-diagnostic","user":{"username":"Alice"}}`, want: ErrIdentityUnavailable},
			{name: "review error with rejected flag", body: `{"error":"private-review-diagnostic"}`, want: ErrIdentityUnavailable},
			{name: "malformed JSON", body: `private-review-diagnostic`, want: ErrIdentityUnavailable},
			{name: "invalid status type", body: `"private-review-diagnostic"`, want: ErrIdentityUnavailable},
			{name: "empty object", response: `{}`, want: ErrIdentityUnavailable},
			{name: "absent status", response: `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview"}`, want: ErrIdentityUnavailable},
			{name: "null status", body: `null`, want: ErrIdentityUnavailable},
			{name: "unexpected response kind", response: `{"apiVersion":"v1","kind":"Status","status":"Success"}`, want: ErrIdentityUnavailable},
			{name: "backend credential rejected", status: http.StatusUnauthorized, want: ErrIdentityUnavailable},
			{name: "review permission denied", status: http.StatusForbidden, want: ErrIdentityUnavailable},
			{name: "reviewer outage", status: http.StatusServiceUnavailable, want: ErrIdentityUnavailable},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := newReviewManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					checkReviewRequest(t, r)
					w.Header().Set("Content-Type", "application/json")
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						fmt.Fprint(w, "private-review-diagnostic") //nolint:errcheck
						return
					}
					if tc.response != "" {
						fmt.Fprint(w, tc.response) //nolint:errcheck
						return
					}
					fmt.Fprintf(w, `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","status":%s}`, tc.body) //nolint:errcheck
				}))
				credential, err := m.Authenticate(t.Context(), "remote-execution-target", reviewRequest())
				if !errors.Is(err, tc.want) {
					t.Fatalf("Authenticate() error = %v, want %v", err, tc.want)
				}
				if err != nil {
					if err.Error() != tc.want.Error() {
						t.Errorf("Authenticate() exposed reviewer details: %v", err)
					}
					if diff := cmp.Diff(ClusterCredential{}, credential); diff != "" {
						t.Errorf("failed authentication returned identity (-want +got):\n%s", diff)
					}
					return
				}
				var status authv1.TokenReviewStatus
				if err := json.Unmarshal([]byte(tc.body), &status); err != nil {
					t.Fatal(err)
				}
				want := ClusterCredential{
					ImpersonateUser: status.User.Username, ImpersonateGroups: status.User.Groups,
					AuthProvider: config.AuthModeOpenShift, IdentityAuthority: m.identityAuthority,
				}
				if diff := cmp.Diff(want, credential); diff != "" {
					t.Errorf("Authenticate() mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("missing or malformed bearer never reaches reviewer", func(t *testing.T) {
		m := newReviewManager(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Error("malformed bearer reached reviewer")
		}))
		for _, header := range []string{"", "Basic secret", "Bearer", "Bearer ", "Bearer  ", "Bearer one two", "Bearer one\ttwo"} {
			req := reviewRequest()
			req.Header.Set("Authorization", header)
			if _, err := m.Authenticate(t.Context(), "unknown-target", req); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("Authenticate(%q) error = %v, want ErrUnauthenticated", header, err)
			}
		}
	})

	t.Run("cache isolates groups and retains expiry during outages", testOpenShiftCache)
}

func TestNewOpenShiftManagerForConfig(t *testing.T) {
	t.Run("rejects redirects without forwarding either token", func(t *testing.T) {
		var redirected atomic.Int64
		destination := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			redirected.Add(1)
		}))
		defer destination.Close()
		for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(http.StatusText(status), func(t *testing.T) {
				m := newReviewManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					checkReviewRequest(t, r)
					http.Redirect(w, r, destination.URL, status)
				}))
				if _, err := m.Authenticate(t.Context(), "target", reviewRequest()); !errors.Is(err, ErrIdentityUnavailable) {
					t.Errorf("redirected Authenticate() error = %v, want ErrIdentityUnavailable", err)
				}
			})
		}
		if redirected.Load() != 0 {
			t.Errorf("redirect destination received %d requests, want 0", redirected.Load())
		}
	})

	t.Run("untrusted TLS fails verification", func(t *testing.T) {
		m := newReviewManager(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Error("untrusted reviewer received a token")
		}), func(cfg *rest.Config) { cfg.CAData = nil })
		if _, err := m.Authenticate(t.Context(), "target", reviewRequest()); !errors.Is(err, ErrIdentityUnavailable) {
			t.Errorf("untrusted Authenticate() error = %v, want ErrIdentityUnavailable", err)
		}
	})
}

func TestReviewToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "caller cancellation"},
		{name: "caller deadline", timeout: 200 * time.Millisecond},
		{name: "review deadline", timeout: tokenReviewTimeout + 5*time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			stopped := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			m := newReviewManager(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				checkReviewRequest(t, r)
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
				close(stopped)
			}))
			ctx, cancel := context.WithCancel(t.Context())
			if tc.timeout != 0 {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), tc.timeout)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := reviewToken(ctx, m.k8sClient.AuthenticationV1().RESTClient(), "caller-review-secret")
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("review did not start")
			}
			if tc.timeout == 0 {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrIdentityUnavailable) {
					t.Errorf("reviewToken() error = %v, want ErrIdentityUnavailable", err)
				}
			case <-time.After(tokenReviewTimeout + 2*time.Second):
				t.Fatal("review did not honor its deadline")
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("reviewer request remained active after cancellation")
			}
		})
	}
}

func TestSanitizeAuthority(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{input: "capp", want: "capp"},
		{input: "Capp issuer", want: "Capp issuer"},
		{input: "corp%prod", want: "corp%prod"},
		{input: "corp/team%prod", want: "corp/team%prod"},
		{input: "capp?issuer#name", want: "capp?issuer#name"},
		{input: "urn:example:issuer", want: "urn:example:issuer"},
		{input: "https://user:secret@home.example:6443/path?token=secret#secret", want: "https://home.example:6443/path"},
		{input: "https://home.example?", want: "https://home.example"},
		{input: "//user@home.example?token=secret#details", want: "//home.example"},
		{input: "https://user:%zz@home.example?secret", want: ""},
		{input: "//user:%zz@home.example?secret", want: ""},
	} {
		if got := sanitizeAuthority(tc.input); got != tc.want {
			t.Errorf("sanitizeAuthority(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func testOpenShiftCache(t *testing.T) {
	t.Helper()
	var unavailable atomic.Bool
	var calls atomic.Int64
	m := newReviewManager(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","status":{"authenticated":true,"user":{"username":"Alice","groups":["Developers"]}}}`) //nolint:errcheck
	}))
	before := time.Now()
	credential, err := m.Authenticate(t.Context(), "first-target", reviewRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := ClusterCredential{
		ImpersonateUser: "Alice", ImpersonateGroups: []string{"Developers"},
		AuthProvider: config.AuthModeOpenShift, IdentityAuthority: m.identityAuthority,
	}
	key := sha256Sum("caller-review-secret")
	entry, _ := m.tokenCache.Load(key)
	cached, ok := entry.(*cachedIdentity)
	if !ok {
		t.Fatalf("cache entry = %T, want *cachedIdentity", entry)
	}
	if cached.expiresAt.Before(before.Add(m.cacheTTL)) || cached.expiresAt.After(time.Now().Add(m.cacheTTL)) {
		t.Fatalf("cache expiry %v does not use configured TTL %v", cached.expiresAt, m.cacheTTL)
	}
	unavailable.Store(true)
	var wg sync.WaitGroup
	// Mutate the fresh result while other requests read and mutate cache hits.
	wg.Go(func() {
		for range 100 {
			credential.ImpersonateGroups[0] = "changed fresh result"
		}
	})
	for range 16 {
		wg.Go(func() {
			got, err := m.Authenticate(t.Context(), "another-target", reviewRequest())
			if err != nil {
				t.Errorf("cached Authenticate() error = %v", err)
				return
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("cached identity changed (-want +got):\n%s", diff)
			}
			got.ImpersonateGroups[0] = "changed cache hit"
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("review calls with a live cache = %d, want 1", calls.Load())
	}
	current, _ := m.tokenCache.Load(key)
	if current != entry {
		t.Error("cache hit replaced the entry or extended its lifetime")
	}
	m.tokenCache.Store(key, &cachedIdentity{
		username: cached.username, groups: cached.groups, expiresAt: time.Now().Add(-time.Second),
	})
	for range 2 {
		if _, err := m.Authenticate(t.Context(), "first-target", reviewRequest()); !errors.Is(err, ErrIdentityUnavailable) {
			t.Errorf("expired cache during outage: error = %v, want ErrIdentityUnavailable", err)
		}
	}
	if calls.Load() != 3 {
		t.Errorf("review calls after failed revalidation = %d, want 3", calls.Load())
	}
	if _, ok := m.tokenCache.Load(key); ok {
		t.Error("failed verification retained an expired identity")
	}
	unavailable.Store(false)
	got, err := m.Authenticate(t.Context(), "first-target", reviewRequest())
	if err != nil {
		t.Fatalf("Authenticate() after reviewer recovery: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("reverified identity (-want +got):\n%s", diff)
	}
	if calls.Load() != 4 {
		t.Errorf("review calls after recovery = %d, want 4", calls.Load())
	}
}

func newReviewManager(t *testing.T, handler http.Handler, configure ...func(*rest.Config)) *openShiftManager {
	t.Helper()
	home := httptest.NewTLSServer(handler)
	t.Cleanup(home.Close)
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			t.Error("identity verification contacted the OAuth server")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(oauth.Close)
	reviewer := &rest.Config{
		Host: home.URL, BearerToken: "backend-review-secret",
		TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: home.Certificate().Raw})},
	}
	for _, change := range configure {
		change(reviewer)
	}
	cfg := &config.Config{}
	cfg.Auth.Mode = config.AuthModeOpenShift
	cfg.Auth.OpenShift.APIServer = oauth.URL
	cfg.Auth.OpenShift.TokenCacheTTLSeconds = 37
	m, err := newOpenShiftManagerForConfig(cfg, reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if m.identityAuthority != home.URL || m.cacheTTL != 37*time.Second {
		t.Fatal("OpenShift construction did not retain home authority or cache TTL")
	}
	return m
}

func reviewRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer caller-review-secret")
	req.Header.Set("X-Auth-Provider", "untrusted-provider")
	req.Header.Set("X-Identity-Authority", "https://untrusted.example")
	return req
}

func checkReviewRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" ||
		r.Header.Get("Authorization") != "Bearer backend-review-secret" {
		t.Errorf("review request has incorrect method, endpoint or backend credentials")
	}
	for name := range r.Header {
		if strings.HasPrefix(name, "Impersonate-") {
			t.Errorf("review request carries impersonation header %q", name)
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading review: %v", err)
		return
	}
	var review authv1.TokenReview
	if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil {
		t.Errorf("decoding review: %v", err)
		return
	}
	if review.Spec.Token != "caller-review-secret" || len(review.Spec.Audiences) != 0 {
		t.Error("review body must contain only the caller token, with audiences unset")
	}
}

func TestTokenReviewDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		upstream error
		want     string
	}{
		{upstream: k8serrors.NewForbidden(authv1.Resource("tokenreviews"), "", errors.New("private-token")), want: "token review failed: status=403 timeout=false"},
		{upstream: fmt.Errorf("private-token: %w", context.DeadlineExceeded), want: "token review failed: status=0 timeout=true"},
		{upstream: errors.New("private-token"), want: "token review failed: status=0 timeout=false"},
	} {
		err := newTokenReviewError(tc.upstream, ErrIdentityUnavailable)
		if got := TokenReviewDiagnostic(fmt.Errorf("wrapped: %w", err)); got != tc.want {
			t.Errorf("TokenReviewDiagnostic() = %q, want %q", got, tc.want)
		}
		if !errors.Is(err, ErrIdentityUnavailable) || err.Error() != ErrIdentityUnavailable.Error() || errors.Is(err, tc.upstream) {
			t.Errorf("review error changed public contract or retained upstream cause: %v", err)
		}
	}
	if got := TokenReviewDiagnostic(ErrUnauthenticated); got != "" {
		t.Errorf("TokenReviewDiagnostic(unrelated) = %q, want empty", got)
	}
}

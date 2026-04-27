package k8s

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

// -- BuildScheme tests --

func TestBuildScheme_Success(t *testing.T) {
	s, err := BuildScheme()
	require.NoError(t, err)
	require.NotNil(t, s)

	assert.True(t, s.IsGroupRegistered(""), "core API group should be registered")
	assert.True(t, s.IsGroupRegistered("rcs.dana.io"), "Capp CRD group should be registered")
}

// -- IsOpenShift tests --

func testRestConfig(t *testing.T, serverURL string) *rest.Config {
	t.Helper()
	return &rest.Config{Host: serverURL}
}

func TestIsOpenShift_200_ReturnsTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/route.openshift.io" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	result, err := IsOpenShift(context.Background(), testRestConfig(t, srv.URL))
	require.NoError(t, err)
	assert.True(t, result)
}

func TestIsOpenShift_404_ReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	result, err := IsOpenShift(context.Background(), testRestConfig(t, srv.URL))
	require.NoError(t, err)
	assert.False(t, result)
}

func TestIsOpenShift_401_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := IsOpenShift(context.Background(), testRestConfig(t, srv.URL))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status 401")
}

func TestIsOpenShift_500_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := IsOpenShift(context.Background(), testRestConfig(t, srv.URL))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status 500")
}

func TestIsOpenShift_HTTPClientError(t *testing.T) {
	cfg := &rest.Config{Host: "http://192.0.2.1:1"}
	_, err := IsOpenShift(context.Background(), cfg)
	assert.Error(t, err)
}

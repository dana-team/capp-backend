package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/pkg/k8s"
	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func GinTestContext(t *testing.T) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return w, c
}

func GinTestContextWithClient(t *testing.T, k8sClient client.Client) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w, c := GinTestContext(t)
	c.Set(string(middleware.K8sClientKey), k8sClient)
	return w, c
}

func GinTestContextWithAllClients(t *testing.T, userClient, adminClient client.Client, meta cluster.ClusterMeta) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w, c := GinTestContext(t)
	c.Set(string(middleware.K8sClientKey), userClient)
	c.Set(string(middleware.AdminK8sClientKey), adminClient)
	c.Set(string(middleware.ClusterMetaKey), meta)
	return w, c
}

func TestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s, err := k8s.BuildScheme()
	if err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	return s
}

// -- Mock AuthManager --

type MockAuthManager struct {
	AuthenticateFn    func(ctx context.Context, clusterName string, r *http.Request) (auth.ClusterCredential, error)
	LoginFn           func(ctx context.Context, clusterName string, token string) (auth.TokenPair, error)
	PasswordLoginFn   func(ctx context.Context, username, password string) (auth.TokenPair, error)
	RefreshFn         func(ctx context.Context, refreshToken string) (auth.TokenPair, error)
	GetAuthorizeURLFn func() (string, error)
	OAuthExchangeFn   func(ctx context.Context, code string) (auth.TokenPair, error)
}

func (m *MockAuthManager) Authenticate(ctx context.Context, clusterName string, r *http.Request) (auth.ClusterCredential, error) {
	if m.AuthenticateFn != nil {
		return m.AuthenticateFn(ctx, clusterName, r)
	}
	return auth.ClusterCredential{}, nil
}

func (m *MockAuthManager) Login(ctx context.Context, clusterName string, token string) (auth.TokenPair, error) {
	if m.LoginFn != nil {
		return m.LoginFn(ctx, clusterName, token)
	}
	return auth.TokenPair{}, auth.ErrNotSupported
}

func (m *MockAuthManager) PasswordLogin(ctx context.Context, username, password string) (auth.TokenPair, error) {
	if m.PasswordLoginFn != nil {
		return m.PasswordLoginFn(ctx, username, password)
	}
	return auth.TokenPair{}, auth.ErrNotSupported
}

func (m *MockAuthManager) Refresh(ctx context.Context, refreshToken string) (auth.TokenPair, error) {
	if m.RefreshFn != nil {
		return m.RefreshFn(ctx, refreshToken)
	}
	return auth.TokenPair{}, auth.ErrNotSupported
}

func (m *MockAuthManager) GetAuthorizeURL() (string, error) {
	if m.GetAuthorizeURLFn != nil {
		return m.GetAuthorizeURLFn()
	}
	return "", nil
}

func (m *MockAuthManager) OAuthExchange(ctx context.Context, code string) (auth.TokenPair, error) {
	if m.OAuthExchangeFn != nil {
		return m.OAuthExchangeFn(ctx, code)
	}
	return auth.TokenPair{}, nil
}

// -- Mock ClusterManager --

type MockClusterManager struct {
	GetFn                func(name string) (*cluster.ClusterClient, error)
	ListFn               func() []cluster.ClusterMeta
	ClientForFn          func(cc *cluster.ClusterClient, cred auth.ClusterCredential) (client.Client, error)
	IsNamespaceAllowedFn func(cc *cluster.ClusterClient, namespace string) bool
	StartHealthChecksFn  func(ctx context.Context, intervalSeconds int)
	IsAnyHealthyFn       func() bool
}

func (m *MockClusterManager) Get(name string) (*cluster.ClusterClient, error) {
	if m.GetFn != nil {
		return m.GetFn(name)
	}
	return nil, cluster.ErrClusterNotFound
}

func (m *MockClusterManager) List() []cluster.ClusterMeta {
	if m.ListFn != nil {
		return m.ListFn()
	}
	return nil
}

func (m *MockClusterManager) ClientFor(cc *cluster.ClusterClient, cred auth.ClusterCredential) (client.Client, error) {
	if m.ClientForFn != nil {
		return m.ClientForFn(cc, cred)
	}
	return nil, nil
}

func (m *MockClusterManager) IsNamespaceAllowed(cc *cluster.ClusterClient, namespace string) bool {
	if m.IsNamespaceAllowedFn != nil {
		return m.IsNamespaceAllowedFn(cc, namespace)
	}
	return true
}

func (m *MockClusterManager) StartHealthChecks(ctx context.Context, intervalSeconds int) {
	if m.StartHealthChecksFn != nil {
		m.StartHealthChecksFn(ctx, intervalSeconds)
	}
}

func (m *MockClusterManager) IsAnyHealthy() bool {
	if m.IsAnyHealthyFn != nil {
		return m.IsAnyHealthyFn()
	}
	return true
}

package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/pkg/k8s"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// FakeClient creates a controller-runtime fake client pre-loaded with the test
// scheme and optional seed objects.
func FakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(TestScheme(t)).WithObjects(objects...).Build()
}

// JSONBody marshals v to JSON and returns it as an *bytes.Buffer suitable for
// use as an HTTP request body.
func JSONBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return bytes.NewBuffer(b)
}

// ServeHTTP sends an HTTP request to the engine and returns the response recorder.
func ServeHTTP(engine *gin.Engine, method, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	engine.ServeHTTP(w, req)
	return w
}

// RouteRegistrar is any type that can register routes on a gin.RouterGroup.
type RouteRegistrar interface {
	RegisterRoutes(rg *gin.RouterGroup)
}

// EngineWithClient creates a Gin test engine that injects k8sClient into the
// context via middleware, then registers the handler's routes.
func EngineWithClient(t *testing.T, k8sClient client.Client, handler RouteRegistrar) *gin.Engine {
	t.Helper()
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), k8sClient)
		c.Next()
	})
	handler.RegisterRoutes(engine.Group(""))
	return engine
}

// EngineWithAdminClient creates a Gin test engine that injects both a user
// client, an admin client, and cluster metadata into the context.
func EngineWithAdminClient(t *testing.T, userClient, adminClient client.Client, meta cluster.ClusterMeta, handler RouteRegistrar) *gin.Engine {
	t.Helper()
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), userClient)
		c.Set(string(middleware.AdminK8sClientKey), adminClient)
		c.Set(string(middleware.ClusterMetaKey), meta)
		c.Next()
	})
	handler.RegisterRoutes(engine.Group(""))
	return engine
}

// EngineHelper wraps a gin.Engine with convenience methods that reduce
// boilerplate in handler tests.
type EngineHelper struct {
	t      *testing.T
	Engine *gin.Engine
}

// NewEngineHelper creates an EngineHelper backed by EngineWithClient.
func NewEngineHelper(t *testing.T, k8sClient client.Client, handler RouteRegistrar) *EngineHelper {
	t.Helper()
	return &EngineHelper{t: t, Engine: EngineWithClient(t, k8sClient, handler)}
}

// NewEngineHelperWithAdmin creates an EngineHelper backed by EngineWithAdminClient.
func NewEngineHelperWithAdmin(t *testing.T, userClient, adminClient client.Client, meta cluster.ClusterMeta, handler RouteRegistrar) *EngineHelper {
	t.Helper()
	return &EngineHelper{t: t, Engine: EngineWithAdminClient(t, userClient, adminClient, meta, handler)}
}

func (h *EngineHelper) Get(path string) *httptest.ResponseRecorder {
	return ServeHTTP(h.Engine, http.MethodGet, path, nil)
}

func (h *EngineHelper) Post(path string, body io.Reader) *httptest.ResponseRecorder {
	return ServeHTTP(h.Engine, http.MethodPost, path, body)
}

func (h *EngineHelper) PostJSON(path string, v any) *httptest.ResponseRecorder {
	return ServeHTTP(h.Engine, http.MethodPost, path, JSONBody(h.t, v))
}

func (h *EngineHelper) Put(path string, body io.Reader) *httptest.ResponseRecorder {
	return ServeHTTP(h.Engine, http.MethodPut, path, body)
}

func (h *EngineHelper) PutJSON(path string, v any) *httptest.ResponseRecorder {
	return ServeHTTP(h.Engine, http.MethodPut, path, JSONBody(h.t, v))
}

func (h *EngineHelper) Delete(path string) *httptest.ResponseRecorder {
	return ServeHTTP(h.Engine, http.MethodDelete, path, nil)
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

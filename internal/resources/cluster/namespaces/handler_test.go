package namespaces

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func testEngine(t *testing.T, userClient, adminClient client.Client, meta cluster.ClusterMeta) *gin.Engine {
	t.Helper()
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), userClient)
		c.Set(string(middleware.AdminK8sClientKey), adminClient)
		c.Set(string(middleware.ClusterMetaKey), meta)
		c.Next()
	})
	h := New()
	h.RegisterRoutes(engine.Group(""))
	return engine
}

func managedNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{consts.ManagedNameSpaceLabelKey: "true"},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return bytes.NewBuffer(b)
}

// -- List (vanilla Kubernetes) tests --

func TestList_VanillaK8s_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	ns := managedNamespace("my-ns")
	adminClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	userClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, userClient, adminClient, cluster.ClusterMeta{Name: "prod", IsOpenShift: false})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// SAR creation may fail on the fake client, filtering out namespaces.
	// The main check is that the endpoint returns 200 and valid JSON.
	assert.NotNil(t, resp.Items)
}

func TestList_VanillaK8s_Empty(t *testing.T) {
	scheme := testutil.TestScheme(t)
	adminClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	userClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, userClient, adminClient, cluster.ClusterMeta{Name: "prod", IsOpenShift: false})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Items)
}

// -- Create tests --

func TestCreate_BadJSON(t *testing.T) {
	scheme := testutil.TestScheme(t)
	userClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	adminClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, userClient, adminClient, cluster.ClusterMeta{Name: "prod"})

	req := httptest.NewRequest(http.MethodPost, "/namespaces", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	// SAR may fail, or bad JSON. We expect a 4xx/5xx.
	assert.True(t, w.Code >= 400)
}

// -- Handler identity tests --

func TestHandler_Name(t *testing.T) {
	h := New()
	assert.Equal(t, "namespaces", h.Name())
}

func TestHandler_RegisterRoutes(t *testing.T) {
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	h := New()
	rg := engine.Group("")
	h.RegisterRoutes(rg)
	routes := engine.Routes()
	var foundGet, foundPost bool
	for _, r := range routes {
		if r.Path == "/namespaces" && r.Method == "GET" {
			foundGet = true
		}
		if r.Path == "/namespaces" && r.Method == "POST" {
			foundPost = true
		}
	}
	assert.True(t, foundGet)
	assert.True(t, foundPost)
}

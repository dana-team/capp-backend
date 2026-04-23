package capps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func testEngine(t *testing.T, k8sClient client.Client) *gin.Engine {
	t.Helper()
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), k8sClient)
		c.Next()
	})
	h := New()
	h.RegisterRoutes(engine.Group(""))
	return engine
}

func makeCapp(name, namespace string) *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return bytes.NewBuffer(b)
}

// -- ListAll tests --

func TestListAll_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(makeCapp("app1", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/capps", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

func TestListAll_Empty(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/capps", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Total)
}

// -- List by namespace tests --

func TestList_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(makeCapp("app1", "ns1"), makeCapp("app2", "ns2")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/capps", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
	assert.Equal(t, "app1", resp.Items[0].Name)
}

// -- Get tests --

func TestGet_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(makeCapp("app1", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/capps/app1", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "app1", resp.Name)
}

func TestGet_NotFound(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/capps/missing", nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Create tests --

func TestCreate_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, CappRequest{Name: "new-app", Namespace: "ns1", Image: "nginx"})
	req := httptest.NewRequest(http.MethodPost, "/namespaces/ns1/capps", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-app", resp.Name)
	assert.Equal(t, "ns1", resp.Namespace)
}

func TestCreate_BadJSON(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	req := httptest.NewRequest(http.MethodPost, "/namespaces/ns1/capps", bytes.NewBufferString("{invalid"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreate_NamespaceOverrideFromURL(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, CappRequest{Name: "app", Namespace: "wrong-ns", Image: "nginx"})
	req := httptest.NewRequest(http.MethodPost, "/namespaces/correct-ns/capps", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "correct-ns", resp.Namespace)
}

// -- Update tests --

func TestUpdate_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	existing := makeCapp("app1", "ns1")
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, CappRequest{Name: "app1", Namespace: "ns1", Image: "nginx:2"})
	req := httptest.NewRequest(http.MethodPut, "/namespaces/ns1/capps/app1", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, CappRequest{Name: "missing", Namespace: "ns1", Image: "nginx"})
	req := httptest.NewRequest(http.MethodPut, "/namespaces/ns1/capps/missing", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	scheme := testutil.TestScheme(t)
	existing := makeCapp("app1", "ns1")
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	engine := testEngine(t, fc)

	req := httptest.NewRequest(http.MethodPut, "/namespaces/ns1/capps/app1", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Delete tests --

func TestDelete_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	existing := makeCapp("app1", "ns1")
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/namespaces/ns1/capps/app1", nil))

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDelete_NotFound(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/namespaces/ns1/capps/missing", nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- respondList tests --

func TestRespondList_CorrectTotalAndMapping(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(makeCapp("a", "ns1"), makeCapp("b", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/capps", nil))

	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Total)
	assert.Len(t, resp.Items, 2)
}

package configmaps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

func managedCM(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{consts.ManagedLabelKey: "true"},
		},
		Data: map[string]string{"key": "value"},
	}
}

func unmanagedCM(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{"key": "value"},
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
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(managedCM("cm1", "ns1"), unmanagedCM("cm2", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/configmaps", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

// -- List by namespace tests --

func TestList_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(managedCM("cm1", "ns1"), managedCM("cm2", "ns2")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/configmaps", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

// -- Get tests --

func TestGet_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedCM("cm1", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/configmaps/cm1", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "cm1", resp.Name)
}

func TestGet_NotFound(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/configmaps/missing", nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGet_MissingManagedLabel(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unmanagedCM("cm1", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/namespaces/ns1/configmaps/cm1", nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Create tests --

func TestCreate_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, ConfigMapRequest{Name: "new-cm", Data: map[string]string{"key": "val"}})
	req := httptest.NewRequest(http.MethodPost, "/namespaces/ns1/configmaps", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp ConfigMapResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-cm", resp.Name)
	assert.Equal(t, "true", resp.Labels[consts.ManagedLabelKey])
}

func TestCreate_BadJSON(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	req := httptest.NewRequest(http.MethodPost, "/namespaces/ns1/configmaps", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Update tests --

func TestUpdate_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedCM("cm1", "ns1")).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, ConfigMapUpdateRequest{Data: map[string]string{"new-key": "new-val"}})
	req := httptest.NewRequest(http.MethodPut, "/namespaces/ns1/configmaps/cm1", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, ConfigMapUpdateRequest{Data: map[string]string{"k": "v"}})
	req := httptest.NewRequest(http.MethodPut, "/namespaces/ns1/configmaps/missing", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_MissingLabel(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unmanagedCM("cm1", "ns1")).Build()
	engine := testEngine(t, fc)

	body := jsonBody(t, ConfigMapUpdateRequest{Data: map[string]string{"k": "v"}})
	req := httptest.NewRequest(http.MethodPut, "/namespaces/ns1/configmaps/cm1", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedCM("cm1", "ns1")).Build()
	engine := testEngine(t, fc)

	req := httptest.NewRequest(http.MethodPut, "/namespaces/ns1/configmaps/cm1", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Delete tests --

func TestDelete_Success(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedCM("cm1", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/namespaces/ns1/configmaps/cm1", nil))

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDelete_NotFound(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/namespaces/ns1/configmaps/missing", nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDelete_MissingLabel(t *testing.T) {
	scheme := testutil.TestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unmanagedCM("cm1", "ns1")).Build()
	engine := testEngine(t, fc)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/namespaces/ns1/configmaps/cm1", nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

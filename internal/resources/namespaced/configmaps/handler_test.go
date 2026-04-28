package configmaps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New())
}

// -- ListAll tests --

func TestListAll_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1"), unmanagedCM("cm2", "ns1")).
		Get("/configmaps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

// -- List by namespace tests --

func TestList_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1"), managedCM("cm2", "ns2")).
		Get("/namespaces/ns1/configmaps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

// -- Get tests --

func TestGet_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1")).Get("/namespaces/ns1/configmaps/cm1")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "cm1", resp.Name)
}

func TestGet_NotFound(t *testing.T) {
	w := engine(t).Get("/namespaces/ns1/configmaps/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGet_MissingManagedLabel(t *testing.T) {
	w := engine(t, unmanagedCM("cm1", "ns1")).Get("/namespaces/ns1/configmaps/cm1")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Create tests --

func TestCreate_Success(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/ns1/configmaps",
		ConfigMapRequest{Name: "new-cm", Data: map[string]string{"key": "val"}})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp ConfigMapResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-cm", resp.Name)
	assert.Equal(t, "true", resp.Labels[consts.ManagedLabelKey])
}

func TestCreate_BadJSON(t *testing.T) {
	w := engine(t).Post("/namespaces/ns1/configmaps", bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Update tests --

func TestUpdate_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1")).PutJSON("/namespaces/ns1/configmaps/cm1",
		ConfigMapUpdateRequest{Data: map[string]string{"new-key": "new-val"}})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	w := engine(t).PutJSON("/namespaces/ns1/configmaps/missing",
		ConfigMapUpdateRequest{Data: map[string]string{"k": "v"}})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_MissingLabel(t *testing.T) {
	w := engine(t, unmanagedCM("cm1", "ns1")).PutJSON("/namespaces/ns1/configmaps/cm1",
		ConfigMapUpdateRequest{Data: map[string]string{"k": "v"}})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1")).Put("/namespaces/ns1/configmaps/cm1",
		bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Delete tests --

func TestDelete_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1")).Delete("/namespaces/ns1/configmaps/cm1")
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDelete_NotFound(t *testing.T) {
	w := engine(t).Delete("/namespaces/ns1/configmaps/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDelete_MissingLabel(t *testing.T) {
	w := engine(t, unmanagedCM("cm1", "ns1")).Delete("/namespaces/ns1/configmaps/cm1")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

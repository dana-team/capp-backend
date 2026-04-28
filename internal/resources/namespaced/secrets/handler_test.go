package secrets

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

func managedSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{consts.ManagedLabelKey: "true"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"key": []byte("value")},
	}
}

func unmanagedSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"key": []byte("value")},
	}
}

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New())
}

// -- ListAll tests --

func TestListAll_Success(t *testing.T) {
	w := engine(t, managedSecret("s1", "ns1"), unmanagedSecret("s2", "ns1")).
		Get("/secrets")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SecretListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

// -- List by namespace tests --

func TestList_Success(t *testing.T) {
	w := engine(t, managedSecret("s1", "ns1"), managedSecret("s2", "ns2")).
		Get("/namespaces/ns1/secrets")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SecretListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

// -- Get tests --

func TestGet_Success(t *testing.T) {
	w := engine(t, managedSecret("s1", "ns1")).Get("/namespaces/ns1/secrets/s1")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SecretResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "s1", resp.Name)
	assert.Equal(t, "value", resp.Data["key"])
}

func TestGet_NotFound(t *testing.T) {
	w := engine(t).Get("/namespaces/ns1/secrets/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGet_MissingManagedLabel(t *testing.T) {
	w := engine(t, unmanagedSecret("s1", "ns1")).Get("/namespaces/ns1/secrets/s1")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Create tests --

func TestCreate_Success(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/ns1/secrets",
		SecretRequest{Name: "new-secret", Data: map[string]string{"key": "val"}})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp SecretResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-secret", resp.Name)
	assert.Equal(t, "true", resp.Labels[consts.ManagedLabelKey])
}

func TestCreate_DefaultOpaqueType(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/ns1/secrets", SecretRequest{Name: "opaque-secret"})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp SecretResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Opaque", resp.Type)
}

func TestCreate_CustomType(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/ns1/secrets",
		SecretRequest{Name: "tls-secret", Type: "kubernetes.io/tls", Data: map[string]string{"tls.crt": "cert", "tls.key": "key"}})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp SecretResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "kubernetes.io/tls", resp.Type)
}

func TestCreate_BadJSON(t *testing.T) {
	w := engine(t).Post("/namespaces/ns1/secrets", bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Update tests --

func TestUpdate_Success(t *testing.T) {
	w := engine(t, managedSecret("s1", "ns1")).PutJSON("/namespaces/ns1/secrets/s1",
		SecretUpdateRequest{Data: map[string]string{"new-key": "new-val"}})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	w := engine(t).PutJSON("/namespaces/ns1/secrets/missing",
		SecretUpdateRequest{Data: map[string]string{"k": "v"}})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_MissingLabel(t *testing.T) {
	w := engine(t, unmanagedSecret("s1", "ns1")).PutJSON("/namespaces/ns1/secrets/s1",
		SecretUpdateRequest{Data: map[string]string{"k": "v"}})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	w := engine(t, managedSecret("s1", "ns1")).Put("/namespaces/ns1/secrets/s1",
		bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Delete tests --

func TestDelete_Success(t *testing.T) {
	w := engine(t, managedSecret("s1", "ns1")).Delete("/namespaces/ns1/secrets/s1")
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDelete_NotFound(t *testing.T) {
	w := engine(t).Delete("/namespaces/ns1/secrets/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDelete_MissingLabel(t *testing.T) {
	w := engine(t, unmanagedSecret("s1", "ns1")).Delete("/namespaces/ns1/secrets/s1")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- bytesToStringMap tests --

func TestBytesToStringMap_Empty(t *testing.T) {
	result := bytesToStringMap(nil)
	assert.NotNil(t, result)
	assert.Empty(t, result)
}

func TestBytesToStringMap_Populated(t *testing.T) {
	result := bytesToStringMap(map[string][]byte{"user": []byte("admin"), "pass": []byte("s3cret")})
	assert.Equal(t, "admin", result["user"])
	assert.Equal(t, "s3cret", result["pass"])
}

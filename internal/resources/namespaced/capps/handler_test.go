package capps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func makeCapp(name, namespace string) *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New())
}

// -- ListAll tests --

func TestListAll_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Get("/capps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

func TestListAll_Empty(t *testing.T) {
	w := engine(t).Get("/capps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Total)
}

// -- List by namespace tests --

func TestList_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1"), makeCapp("app2", "ns2")).
		Get("/namespaces/ns1/capps")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
	assert.Equal(t, "app1", resp.Items[0].Name)
}

// -- Get tests --

func TestGet_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Get("/namespaces/ns1/capps/app1")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "app1", resp.Name)
}

func TestGet_NotFound(t *testing.T) {
	w := engine(t).Get("/namespaces/ns1/capps/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Create tests --

func TestCreate_Success(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/ns1/capps",
		CappRequest{Name: "new-app", Namespace: "ns1", Image: "nginx"})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-app", resp.Name)
	assert.Equal(t, "ns1", resp.Namespace)
}

func TestCreate_BadJSON(t *testing.T) {
	w := engine(t).Post("/namespaces/ns1/capps", bytes.NewBufferString("{invalid"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreate_NamespaceOverrideFromURL(t *testing.T) {
	w := engine(t).PostJSON("/namespaces/correct-ns/capps",
		CappRequest{Name: "app", Namespace: "wrong-ns", Image: "nginx"})

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "correct-ns", resp.Namespace)
}

// -- Update tests --

func TestUpdate_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).PutJSON("/namespaces/ns1/capps/app1",
		CappRequest{Name: "app1", Namespace: "ns1", Image: "nginx:2"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	w := engine(t).PutJSON("/namespaces/ns1/capps/missing",
		CappRequest{Name: "missing", Namespace: "ns1", Image: "nginx"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Put("/namespaces/ns1/capps/app1",
		bytes.NewBufferString("{bad"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Delete tests --

func TestDelete_Success(t *testing.T) {
	w := engine(t, makeCapp("app1", "ns1")).Delete("/namespaces/ns1/capps/app1")
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDelete_NotFound(t *testing.T) {
	w := engine(t).Delete("/namespaces/ns1/capps/missing")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- respondList tests --

func TestRespondList_CorrectTotalAndMapping(t *testing.T) {
	w := engine(t, makeCapp("a", "ns1"), makeCapp("b", "ns1")).
		Get("/namespaces/ns1/capps")

	var resp CappListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Total)
	assert.Len(t, resp.Items, 2)
}

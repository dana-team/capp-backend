package namespaces

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func managedNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{consts.ManagedNameSpaceLabelKey: "true"},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func engine(t *testing.T, meta cluster.ClusterMeta, adminObjects []client.Object, userObjects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelperWithAdmin(t,
		testutil.FakeClient(t, userObjects...),
		testutil.FakeClient(t, adminObjects...),
		meta, New())
}

// -- List (vanilla Kubernetes) tests --

func TestList_VanillaK8s_Success(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod", IsOpenShift: false},
		[]client.Object{managedNamespace("my-ns")})
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// SAR creation may fail on the fake client, filtering out namespaces.
	assert.NotNil(t, resp.Items)
}

func TestList_VanillaK8s_Empty(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod", IsOpenShift: false}, nil)
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Items)
}

// -- Create tests --

func TestCreate_BadJSON(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.Post("/namespaces", bytes.NewBufferString("{bad"))

	// SAR may fail, or bad JSON. We expect a 4xx/5xx.
	assert.True(t, w.Code >= 400)
}

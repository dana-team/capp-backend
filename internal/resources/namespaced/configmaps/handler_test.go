package configmaps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/boundcapps"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func managedCM(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue},
		},
		Data: map[string]string{"key": "value"},
	}
}

func unmanagedCM(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"},
		Data:       map[string]string{"key": "value"},
	}
}

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New())
}

// -- ListAll tests --

func TestListAll_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1"), unmanagedCM("cm2")).
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

// -- ListNames tests --

func TestListNames_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1"), managedCM("cm2", "ns1"), managedCM("cm3", "ns2")).
		Get("/namespaces/ns1/configmaps/names")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapNameListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Total)
	assert.ElementsMatch(t, []string{"cm1", "cm2"}, resp.Items)
}

func TestListNames_ExcludesUnmanaged(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1"), unmanagedCM("cm2")).
		Get("/namespaces/ns1/configmaps/names")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapNameListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
	assert.Equal(t, []string{"cm1"}, resp.Items)
}

func TestListNames_Empty(t *testing.T) {
	w := engine(t).Get("/namespaces/ns1/configmaps/names")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"items":[]`)
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
	w := engine(t, unmanagedCM("cm1")).Get("/namespaces/ns1/configmaps/cm1")
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
	assert.Equal(t, consts.ManagedLabelValue, resp.Labels[consts.ManagedLabelKey])
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
	w := engine(t, unmanagedCM("cm1")).PutJSON("/namespaces/ns1/configmaps/cm1",
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
	w := engine(t, unmanagedCM("cm1")).Delete("/namespaces/ns1/configmaps/cm1")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// -- Bound Capp tests --

func boundCapp(name string, reload bool) *cappv1alpha1.Capp {
	capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1"}}
	if reload {
		capp.Annotations = map[string]string{consts.BoundResourceRolloutAnnotation: "true"}
	}
	capp.Spec.ConfigurationSpec.Template.Spec.PodSpec = corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm1"}}}}}}
	return capp
}

func getCapp(t *testing.T, k8sClient client.Client, name string) *cappv1alpha1.Capp {
	t.Helper()
	capp := &cappv1alpha1.Capp{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: name}, capp))
	return capp
}

func TestBound_Success(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1"), boundCapp("reloads", true), boundCapp("static", false)).
		Get("/namespaces/ns1/configmaps/cm1/bound")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp boundcapps.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"reloads", "static"}, resp.BoundCapps)
	assert.Equal(t, []string{"reloads"}, resp.ToReload)
}

func TestBound_MissingLabel(t *testing.T) {
	w := engine(t, unmanagedCM("cm1")).Get("/namespaces/ns1/configmaps/cm1/bound")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// resourceVersionOf returns the stored resourceVersion of the cm1 object.
func resourceVersionOf(t *testing.T, k8sClient client.Client) string {
	t.Helper()
	obj := &corev1.ConfigMap{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "cm1"}, obj))
	return obj.ResourceVersion
}

func boundVersions(t *testing.T, capp *cappv1alpha1.Capp) map[string]string {
	t.Helper()
	versions := map[string]string{}
	require.NoError(t, json.Unmarshal([]byte(capp.Annotations[consts.BoundResourceVersionsAnnotation]), &versions))
	return versions
}

func TestUpdate_RestartsOptedInCapps(t *testing.T) {
	k8sClient := testutil.FakeClient(t, managedCM("cm1", "ns1"), boundCapp("reloads", true), boundCapp("static", false))
	w := testutil.NewEngineHelper(t, k8sClient, New()).PutJSON("/namespaces/ns1/configmaps/cm1",
		ConfigMapUpdateRequest{Data: map[string]string{"key": "changed"}})
	require.Equal(t, http.StatusOK, w.Code)

	reloads := getCapp(t, k8sClient, "reloads")
	assert.Equal(t, map[string]string{"ConfigMap/cm1": resourceVersionOf(t, k8sClient)}, boundVersions(t, reloads))
	assert.NotEmpty(t, reloads.Annotations[consts.RestartedAtAnnotation])
	assert.Equal(t, "ConfigMap/cm1", reloads.Annotations[consts.RestartReasonAnnotation])
	assert.NotContains(t, getCapp(t, k8sClient, "static").Annotations, consts.BoundResourceVersionsAnnotation)
}

// Repeating a successful PUT must not restart the Capps again.
func TestUpdate_RepeatedPutDoesNotRestartAgain(t *testing.T) {
	k8sClient := testutil.FakeClient(t, managedCM("cm1", "ns1"), boundCapp("reloads", true))
	h := testutil.NewEngineHelper(t, k8sClient, New())
	body := ConfigMapUpdateRequest{Data: map[string]string{"key": "changed"}}

	require.Equal(t, http.StatusOK, h.PutJSON("/namespaces/ns1/configmaps/cm1", body).Code)
	before := getCapp(t, k8sClient, "reloads").ResourceVersion

	require.Equal(t, http.StatusOK, h.PutJSON("/namespaces/ns1/configmaps/cm1", body).Code)
	assert.Equal(t, before, getCapp(t, k8sClient, "reloads").ResourceVersion)
}

// Two changes in a row each restart the Capp, however close together they are.
func TestUpdate_EachChangeRestarts(t *testing.T) {
	k8sClient := testutil.FakeClient(t, managedCM("cm1", "ns1"), boundCapp("reloads", true))
	h := testutil.NewEngineHelper(t, k8sClient, New())

	require.Equal(t, http.StatusOK, h.PutJSON("/namespaces/ns1/configmaps/cm1", ConfigMapUpdateRequest{Data: map[string]string{"key": "one"}}).Code)
	first := getCapp(t, k8sClient, "reloads").Annotations[consts.BoundResourceVersionsAnnotation]
	require.Equal(t, http.StatusOK, h.PutJSON("/namespaces/ns1/configmaps/cm1", ConfigMapUpdateRequest{Data: map[string]string{"key": "two"}}).Code)
	second := getCapp(t, k8sClient, "reloads").Annotations[consts.BoundResourceVersionsAnnotation]

	assert.NotEqual(t, first, second)
	assert.Equal(t, map[string]string{"ConfigMap/cm1": resourceVersionOf(t, k8sClient)}, boundVersions(t, getCapp(t, k8sClient, "reloads")))
}

// A PUT whose restart failed returns 500; retrying the same PUT restarts the
// Capp even though the data no longer changes.
func TestUpdate_RetryAfterFailedRestart(t *testing.T) {
	failCappUpdates := true
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, isCapp := obj.(*cappv1alpha1.Capp); isCapp && failCappUpdates {
				return errors.New("boom")
			}
			return c.Update(ctx, obj, opts...)
		},
	}, managedCM("cm1", "ns1"), boundCapp("reloads", true))
	h := testutil.NewEngineHelper(t, k8sClient, New())
	body := ConfigMapUpdateRequest{Data: map[string]string{"key": "changed"}}

	w := h.PutJSON("/namespaces/ns1/configmaps/cm1", body)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, getCapp(t, k8sClient, "reloads").Annotations, consts.BoundResourceVersionsAnnotation)

	failCappUpdates = false
	require.Equal(t, http.StatusOK, h.PutJSON("/namespaces/ns1/configmaps/cm1", body).Code)
	assert.Equal(t, map[string]string{"ConfigMap/cm1": resourceVersionOf(t, k8sClient)}, boundVersions(t, getCapp(t, k8sClient, "reloads")))
}

func TestDelete_InUseReturns409(t *testing.T) {
	w := engine(t, managedCM("cm1", "ns1"), boundCapp("static", false)).Delete("/namespaces/ns1/configmaps/cm1")
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "static")
}

package revisions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cappSpec(image string) cappv1alpha1.CappSpec {
	var spec cappv1alpha1.CappSpec
	spec.ConfigurationSpec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: image}}
	return spec
}

// makeCapp returns the live Capp ns1/app1 (image nginx:2) that the revisions in these tests belong to.
func makeCapp(labels map[string]string) *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "app1", Namespace: "ns1", Labels: labels},
		Spec:       cappSpec("nginx:2"),
	}
}

func makeRevision(cappName, namespace string, number int, image string, labels map[string]string) *cappv1alpha1.CappRevision {
	return &cappv1alpha1.CappRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%05d", cappName, number),
			Namespace: namespace,
			Labels:    map[string]string{k8s.LabelCappName: cappName},
		},
		Spec: cappv1alpha1.CappRevisionSpec{
			RevisionNumber: number,
			CappTemplate: cappv1alpha1.CappTemplate{
				Spec:        cappSpec(image),
				Labels:      labels,
				Annotations: map[string]string{"note": fmt.Sprintf("rev-%d", number)},
			},
		},
	}
}

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), New(config.CappSizes{}))
}

func TestList_SortedNewestFirstAndFilteredByCapp(t *testing.T) {
	w := engine(t,
		makeRevision("app1", "ns1", 1, "nginx:1", nil),
		makeRevision("app1", "ns1", 3, "nginx:3", nil),
		makeRevision("app1", "ns1", 2, "nginx:2", nil),
		makeRevision("other", "ns1", 1, "busybox", nil),
		makeRevision("app1", "ns2", 9, "nginx:9", nil),
	).Get("/namespaces/ns1/capps/app1/revisions")

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappRevisionListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 3, resp.Total)
	assert.Equal(t, []int{3, 2, 1}, []int{resp.Items[0].RevisionNumber, resp.Items[1].RevisionNumber, resp.Items[2].RevisionNumber})
	assert.Equal(t, "nginx:3", resp.Items[0].Image)
	assert.Equal(t, "app1-00003", resp.Items[0].Name)
	assert.Equal(t, "rev-3", resp.Items[0].Annotations["note"])
}

func TestList_Empty(t *testing.T) {
	w := engine(t).Get("/namespaces/ns1/capps/app1/revisions")

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappRevisionListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Total)
	assert.NotNil(t, resp.Items)
}

func TestGet(t *testing.T) {
	w := engine(t,
		makeRevision("app1", "ns1", 1, "nginx:1", nil),
		makeRevision("app1", "ns1", 2, "nginx:2", nil),
	).Get("/namespaces/ns1/capps/app1/revisions/1")

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappRevisionResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.RevisionNumber)
	assert.Equal(t, "nginx:1", resp.Image)
}

func TestGet_NotFound(t *testing.T) {
	w := engine(t, makeRevision("app1", "ns1", 1, "nginx:1", nil)).
		Get("/namespaces/ns1/capps/app1/revisions/7")

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGet_InvalidRevision(t *testing.T) {
	for _, rev := range []string{"abc", "0", "-1"} {
		t.Run(rev, func(t *testing.T) {
			w := engine(t).Get("/namespaces/ns1/capps/app1/revisions/" + rev)
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestRollback_AppliesTemplate(t *testing.T) {
	k8sClient := testutil.FakeClient(t,
		makeCapp(map[string]string{"team": "b"}),
		makeRevision("app1", "ns1", 1, "nginx:1", map[string]string{"team": "a"}),
	)
	w := testutil.NewEngineHelper(t, k8sClient, New(config.CappSizes{})).
		Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	require.Equal(t, http.StatusOK, w.Code)
	var resp capps.CappResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "app1", resp.Name)
	assert.Equal(t, "nginx:1", resp.Image)

	var live cappv1alpha1.Capp
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "app1"}, &live))
	assert.Equal(t, "nginx:1", live.Spec.ConfigurationSpec.Template.Spec.Containers[0].Image)
	assert.Equal(t, map[string]string{"team": "a"}, live.Labels)
	assert.Equal(t, "rev-1", live.Annotations["note"])
}

func TestRollback_RevisionNotFound(t *testing.T) {
	w := engine(t, makeCapp(nil)).
		Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRollback_CappNotFound(t *testing.T) {
	w := engine(t, makeRevision("app1", "ns1", 1, "nginx:1", nil)).
		Post("/namespaces/ns1/capps/app1/revisions/1/rollback", nil)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "Capp")
}

func TestApplyTemplate_StripsRolloutAnnotation(t *testing.T) {
	live := &cappv1alpha1.Capp{}
	tmpl := cappv1alpha1.CappTemplate{Annotations: map[string]string{
		k8s.AnnotationRollout: "abort:app-00002",
		"keep":                "me",
	}}

	updated := applyTemplate(live, tmpl)

	assert.Equal(t, map[string]string{"keep": "me"}, updated.Annotations)
}

package tracking_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/internal/resources/tracking/trackingtest"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const adoptPath = "/api/v1/clusters/c1/namespaces/team-a/drift/adopt"

func kinds() []tracking.AdoptKind {
	managed := []client.ListOption{client.MatchingLabels{consts.ManagedLabelKey: consts.ManagedLabelValue}}
	return []tracking.AdoptKind{
		{Kind: desiredstate.KindCapp, NewList: func() client.ObjectList { return &cappv1alpha1.CappList{} }},
		{Kind: desiredstate.KindSecret, NewList: func() client.ObjectList { return &corev1.SecretList{} }, Options: managed},
	}
}

func secret(namespace, name string, managed bool) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if managed {
		s.Labels = map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}
	}
	return s
}

func adopt(t *testing.T, e *testutil.EngineHelper) drift.AdoptResponse {
	t.Helper()
	w := e.Post(adoptPath, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp drift.AdoptResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func TestAdopt(t *testing.T) {
	capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app"}}
	k8sClient := testutil.FakeClient(t, capp,
		secret("team-a", "db", true), secret("team-a", "service-account-token", false), secret("team-b", "other", true))
	tracker, store := trackingtest.New(t)
	e := trackingtest.Engine(t, k8sClient, tracking.NewAdopter(tracker, kinds()...))

	resp := adopt(t, e)
	assert.Equal(t, []drift.AdoptedItem{{Kind: "Capp", Name: "app"}, {Kind: "Secret", Name: "db"}}, resp.Adopted)
	assert.Equal(t, 2, store.Len())

	// Adopting again never replaces a desired state, even a drifted one.
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKeyFromObject(capp), capp))
	capp.Spec.State = "disabled"
	require.NoError(t, k8sClient.Update(t.Context(), capp))
	assert.Empty(t, adopt(t, e).Adopted)
	record, err := store.Get(t.Context(), desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindCapp, Namespace: "team-a", Name: "app"})
	require.NoError(t, err)
	assert.NotContains(t, record.Desired.JSON, "disabled")
	assert.Equal(t, desiredstate.SourceAccept, record.Source)
}

func TestAdoptSkipsForbiddenKinds(t *testing.T) {
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.SecretList); ok {
				return k8serrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", nil)
			}
			return c.List(ctx, list, opts...)
		},
	}, &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app"}}, secret("team-a", "db", true))
	tracker, _ := trackingtest.New(t)
	e := trackingtest.Engine(t, k8sClient, tracking.NewAdopter(tracker, kinds()...))
	assert.Equal(t, []drift.AdoptedItem{{Kind: "Capp", Name: "app"}}, adopt(t, e).Adopted)
}

func TestAdoptUnavailable(t *testing.T) {
	tracker, store := trackingtest.New(t)
	store.Err = assert.AnError
	e := trackingtest.Engine(t, testutil.FakeClient(t), tracking.NewAdopter(tracker, kinds()...))
	require.Equal(t, http.StatusServiceUnavailable, e.Post(adoptPath, nil).Code)

	e = trackingtest.Engine(t, testutil.FakeClient(t), tracking.NewAdopter(nil))
	require.Equal(t, http.StatusNotImplemented, e.Post(adoptPath, nil).Code)
}

package capps

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/desiredstate/desiredstatetest"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/tracking/trackingtest"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	trackedList = "/api/v1/clusters/c1/namespaces/team-a/capps"
	trackedItem = trackedList + "/app"
)

type trackedEngine struct {
	*testutil.EngineHelper
	t     *testing.T
	k8s   client.Client
	store *desiredstatetest.MemoryStore
}

func newTrackedEngine(t *testing.T, mgr cluster.ClusterManager, objects ...client.Object) *trackedEngine {
	t.Helper()
	tracker, store := trackingtest.New(t)
	k8sClient := testutil.FakeClient(t, objects...)
	handler := mustHandler(t, mgr, makeSizes()).WithTracker(tracker)
	return &trackedEngine{trackingtest.Engine(t, k8sClient, handler), t, k8sClient, store}
}

func (e *trackedEngine) create() {
	e.t.Helper()
	w := e.PostJSON(trackedList, CappRequest{Name: "app", Image: "web:1", ScaleSpec: ScaleSpec{MaxReplicas: ptr.To[int32](5)}})
	require.Equal(e.t, http.StatusCreated, w.Code, w.Body.String())
}

func (e *trackedEngine) live() *cappv1alpha1.Capp {
	e.t.Helper()
	capp := &cappv1alpha1.Capp{}
	require.NoError(e.t, e.k8s.Get(e.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "app"}, capp))
	return capp
}

func (e *trackedEngine) edit(change func(*cappv1alpha1.Capp)) {
	e.t.Helper()
	capp := e.live()
	change(capp)
	require.NoError(e.t, e.k8s.Update(e.t.Context(), capp))
}

func liveContainer(c *cappv1alpha1.Capp) *corev1.Container {
	return &c.Spec.ConfigurationSpec.Template.Spec.Containers[0]
}

func TestTrackedCappUpdateAsksAboutDrift(t *testing.T) {
	// args is not part of the API, so an update can only keep or drop it.
	drifted := func(t *testing.T) *trackedEngine {
		e := newTrackedEngine(t, nil)
		e.create()
		e.edit(func(c *cappv1alpha1.Capp) {
			c.Spec.ScaleSpec.MaxReplicas = ptr.To[int32](10)
			liveContainer(c).Args = []string{"--debug"}
		})
		return e
	}
	update := CappRequest{Name: "app", Image: "web:2", ScaleSpec: ScaleSpec{MaxReplicas: ptr.To[int32](5)}}

	e := drifted(t)
	w := e.PutJSON(trackedItem, update)
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Len(t, trackingtest.Drift(t, w).Fields, 2)
	assert.Equal(t, "web:1", liveContainer(e.live()).Image)

	for onDrift, wantArgs := range map[string][]string{drift.OnDriftAccept: {"--debug"}, drift.OnDriftRevert: nil} {
		e := drifted(t)
		w := e.PutJSON(trackedItem+"?onDrift="+onDrift, update)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		live := e.live()
		assert.Equal(t, "web:2", liveContainer(live).Image)
		assert.Equal(t, int32(5), *live.Spec.ScaleSpec.MaxReplicas)
		assert.Equal(t, wantArgs, liveContainer(live).Args, onDrift)
		assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, e.Get(trackedItem)).State)
	}
}

func TestTrackedCappListReportsDrift(t *testing.T) {
	e := newTrackedEngine(t, nil, makeCapp("legacy", "team-a"), makeCapp("elsewhere", "team-b"))
	e.create()
	e.edit(func(c *cappv1alpha1.Capp) { liveContainer(c).Image = "web:9" })

	var list CappListResponse
	require.NoError(t, json.Unmarshal(e.Get(trackedList).Body.Bytes(), &list))
	states := map[string]string{}
	for _, item := range list.Items {
		states[item.Name] = item.Drift.State
	}
	assert.Equal(t, map[string]string{"app": drift.StateChanged, "legacy": drift.StateUnmanaged}, states)

	require.NoError(t, e.k8s.Delete(t.Context(), e.live()))
	list = CappListResponse{}
	require.NoError(t, json.Unmarshal(e.Get("/api/v1/clusters/c1/capps").Body.Bytes(), &list))
	assert.Equal(t, 2, list.Total)
	require.Len(t, list.Missing, 1)
	assert.Equal(t, "app", list.Missing[0].Name)
}

func TestTrackedCappRestartIsNotDrift(t *testing.T) {
	e := newTrackedEngine(t, nil)
	e.create()
	e.edit(func(c *cappv1alpha1.Capp) {
		c.Annotations = map[string]string{
			consts.BoundResourceVersionsAnnotation: `{"Secret/db":"5"}`,
			consts.RestartedAtAnnotation:           "2026-01-01T00:00:00Z",
			k8s.AnnotationRollout:                  "promote:app-00002",
		}
	})
	assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, e.Get(trackedItem)).State)
}

func TestTrackedCappTerminating(t *testing.T) {
	now := metav1.Now()
	capp := makeCapp("app", "team-a")
	capp.DeletionTimestamp, capp.Finalizers = &now, []string{"dana.io/cleanup"}
	e := newTrackedEngine(t, nil, capp)
	assert.Nil(t, trackingtest.Drift(t, e.Get(trackedItem)))
	require.Equal(t, http.StatusConflict, e.PostJSON(trackedItem+"/drift/accept", drift.ActionRequest{}).Code)
}

func TestTrackedCappStrategyRemoval(t *testing.T) {
	e := newTrackedEngine(t, nil)
	e.create()
	e.edit(func(c *cappv1alpha1.Capp) {
		c.Spec.RouteSpec.RolloutStrategy = &cappv1alpha1.RolloutStrategy{Stages: []cappv1alpha1.RolloutStage{{Percent: 100}}}
	})
	d := trackingtest.Drift(t, e.Get(trackedItem))
	require.Equal(t, http.StatusOK, e.PostJSON(trackedItem+"/drift/accept", trackingtest.Action(d)).Code)

	require.Equal(t, http.StatusOK, e.Delete(trackedItem+"/rollout/strategy").Code)
	assert.Nil(t, e.live().Spec.RouteSpec.RolloutStrategy)
	assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, e.Get(trackedItem)).State)
}

func TestTrackedCappMigration(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "team-a", Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "settings", Namespace: "team-a", Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue},
	}}
	target := testutil.FakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}})
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c2"}}
	cc.SetHealthy(true)
	mgr := &testutil.MockClusterManager{
		GetFn:                func(string) (*cluster.ClusterClient, error) { return cc, nil },
		IsNamespaceAllowedFn: func(*cluster.ClusterClient, string) bool { return true },
		ClientForFn:          func(*cluster.ClusterClient, auth.ClusterCredential) (client.Client, error) { return target, nil },
	}
	e := newTrackedEngine(t, mgr, secret, configMap)
	e.create()

	w := e.PostJSON(trackedItem+"/migrate", MigrateRequest{TargetCluster: "c2", TargetNamespace: "team-b", DeleteSource: true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "warnings")

	for _, ref := range []desiredstate.Ref{
		{Cluster: "c2", Kind: desiredstate.KindCapp, Namespace: "team-b", Name: "app"},
		{Cluster: "c2", Kind: desiredstate.KindSecret, Namespace: "team-b", Name: "db"},
		{Cluster: "c2", Kind: desiredstate.KindConfigMap, Namespace: "team-b", Name: "settings"},
	} {
		_, err := e.store.Get(t.Context(), ref)
		require.NoError(t, err, ref.ID())
	}
	_, err := e.store.Get(t.Context(), desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindCapp, Namespace: "team-a", Name: "app"})
	require.ErrorIs(t, err, desiredstate.ErrNotFound)
}

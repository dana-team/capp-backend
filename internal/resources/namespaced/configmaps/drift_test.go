package configmaps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/desiredstate/desiredstatetest"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/tracking/trackingtest"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	listPath = "/api/v1/clusters/c1/namespaces/team-a/configmaps"
	itemPath = listPath + "/settings"
)

var settingsRef = desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindConfigMap, Namespace: "team-a", Name: "settings"}

type driftEngine struct {
	*testutil.EngineHelper
	t     *testing.T
	k8s   client.Client
	store *desiredstatetest.MemoryStore
}

func newDriftEngine(t *testing.T, k8sClient client.Client) *driftEngine {
	t.Helper()
	tracker, store := trackingtest.New(t)
	return &driftEngine{trackingtest.Engine(t, k8sClient, New().WithTracker(tracker)), t, k8sClient, store}
}

func (e *driftEngine) create(data map[string]string) {
	e.t.Helper()
	w := e.PostJSON(listPath, ConfigMapRequest{Name: "settings", Data: data})
	require.Equal(e.t, http.StatusCreated, w.Code, w.Body.String())
}

// edit changes the live object the way kubectl would.
func (e *driftEngine) edit(change func(*corev1.ConfigMap)) {
	e.t.Helper()
	cm := e.live()
	change(cm)
	require.NoError(e.t, e.k8s.Update(e.t.Context(), cm))
}

func (e *driftEngine) live() *corev1.ConfigMap {
	e.t.Helper()
	cm := &corev1.ConfigMap{}
	require.NoError(e.t, e.k8s.Get(e.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "settings"}, cm))
	return cm
}

func (e *driftEngine) drift() *drift.Drift {
	e.t.Helper()
	return trackingtest.Drift(e.t, e.Get(itemPath))
}

func TestDriftLifecycle(t *testing.T) {
	e := newDriftEngine(t, testutil.FakeClient(t))
	e.create(map[string]string{"a": "1"})
	assert.Equal(t, drift.StateInSync, e.drift().State)

	e.edit(func(cm *corev1.ConfigMap) { cm.Data["a"] = "2" })
	d := e.drift()
	assert.Equal(t, drift.StateChanged, d.State)
	assert.Equal(t, []drift.Field{{Path: "/data/a", Desired: "1", Live: "2"}}, d.Fields)

	w := e.PostJSON(itemPath+"/drift/revert", trackingtest.Action(d))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, w).State)
	assert.Equal(t, map[string]string{"a": "1"}, e.live().Data)

	e.edit(func(cm *corev1.ConfigMap) { cm.Data["a"] = "3" })
	w = e.PostJSON(itemPath+"/drift/accept", trackingtest.Action(e.drift()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, w).State)
	assert.Equal(t, "3", e.live().Data["a"])
	assert.Equal(t, drift.StateInSync, e.drift().State)
}

func TestDriftActionsRejectStaleRequests(t *testing.T) {
	for _, action := range []string{"revert", "accept"} {
		t.Run(action, func(t *testing.T) {
			e := newDriftEngine(t, testutil.FakeClient(t))
			e.create(map[string]string{"a": "1"})
			e.edit(func(cm *corev1.ConfigMap) { cm.Data["a"] = "2" })
			seen := trackingtest.Action(e.drift())
			e.edit(func(cm *corev1.ConfigMap) { cm.Data["a"] = "3" })

			w := e.PostJSON(itemPath+"/drift/"+action, seen)
			require.Equal(t, http.StatusConflict, w.Code)
			current := trackingtest.Drift(t, w)
			assert.NotEqual(t, seen.LiveVersion, current.LiveVersion)
			assert.Equal(t, "3", e.live().Data["a"])

			w = e.PostJSON(itemPath+"/drift/"+action, drift.ActionRequest{Generation: 99, LiveVersion: current.LiveVersion})
			require.Equal(t, http.StatusConflict, w.Code)
			require.Equal(t, http.StatusBadRequest, e.Post(itemPath+"/drift/"+action, nil).Code)
		})
	}
}

func TestMissingObject(t *testing.T) {
	missing := func(t *testing.T) (*driftEngine, *drift.Drift) {
		e := newDriftEngine(t, testutil.FakeClient(t))
		e.create(map[string]string{"a": "1"})
		require.NoError(t, e.k8s.Delete(t.Context(), e.live()))
		w := e.Get(itemPath)
		require.Equal(t, http.StatusNotFound, w.Code)
		d := trackingtest.Drift(t, w)
		require.NotNil(t, d)
		assert.Equal(t, drift.StateMissing, d.State)
		return e, d
	}

	t.Run("list", func(t *testing.T) {
		e, _ := missing(t)
		for _, path := range []string{listPath, "/api/v1/clusters/c1/configmaps"} {
			var list ConfigMapListResponse
			require.NoError(t, json.Unmarshal(e.Get(path).Body.Bytes(), &list))
			assert.Empty(t, list.Items)
			assert.Zero(t, list.Total)
			require.Len(t, list.Missing, 1)
			assert.Equal(t, "settings", list.Missing[0].Name)
		}
		var other ConfigMapListResponse
		require.NoError(t, json.Unmarshal(e.Get("/api/v1/clusters/c1/namespaces/team-b/configmaps").Body.Bytes(), &other))
		assert.Empty(t, other.Missing)
	})

	t.Run("revert recreates", func(t *testing.T) {
		e, d := missing(t)
		w := e.PostJSON(itemPath+"/drift/revert", trackingtest.Action(d))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, map[string]string{"a": "1"}, e.live().Data)
		assert.True(t, isManaged(e.live()))
	})

	t.Run("accept keeps the backup", func(t *testing.T) {
		e, d := missing(t)
		w := e.PostJSON(itemPath+"/drift/accept", trackingtest.Action(d))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Nil(t, trackingtest.Drift(t, w))

		record, err := e.store.Get(t.Context(), settingsRef)
		require.NoError(t, err)
		assert.False(t, record.Active())
		assert.Contains(t, record.Desired.JSON, `"a":"1"`)

		w = e.Get(itemPath)
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Nil(t, trackingtest.Drift(t, w))

		e.create(map[string]string{"a": "new"})
		assert.Equal(t, drift.StateInSync, e.drift().State)
	})
}

func TestUnmanagedObject(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "settings", Namespace: "team-a", Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue},
	}}
	e := newDriftEngine(t, testutil.FakeClient(t, cm))
	d := e.drift()
	assert.Equal(t, drift.StateUnmanaged, d.State)

	require.Equal(t, http.StatusNotFound, e.PostJSON(itemPath+"/drift/revert", trackingtest.Action(d)).Code)
	e.live()

	require.Equal(t, http.StatusOK, e.PostJSON(itemPath+"/drift/accept", trackingtest.Action(d)).Code)
	assert.Equal(t, drift.StateInSync, e.drift().State)
}

func TestUnlabelledObjectsAreNotExposed(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "team-a"}, Data: map[string]string{"private": "value"}}
	e := newDriftEngine(t, testutil.FakeClient(t, cm))
	w := e.Get(itemPath)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Nil(t, trackingtest.Drift(t, w))
	for _, action := range []string{"revert", "accept"} {
		w := e.PostJSON(itemPath+"/drift/"+action, drift.ActionRequest{})
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.Nil(t, trackingtest.Drift(t, w))
	}
	assert.Zero(t, e.store.Len())
}

func TestRevertRestoresTheManagedLabel(t *testing.T) {
	e := newDriftEngine(t, testutil.FakeClient(t))
	e.create(map[string]string{"a": "1"})
	e.edit(func(cm *corev1.ConfigMap) { cm.Labels = nil })
	w := e.Get(itemPath)
	require.Equal(t, http.StatusNotFound, w.Code)
	d := trackingtest.Drift(t, w)
	require.NotNil(t, d)
	assert.Equal(t, drift.StateChanged, d.State)
	require.Equal(t, http.StatusOK, e.PostJSON(itemPath+"/drift/revert", trackingtest.Action(d)).Code)
	assert.True(t, isManaged(e.live()))
}

func TestUpdateOfDriftedObject(t *testing.T) {
	drifted := func(t *testing.T) *driftEngine {
		e := newDriftEngine(t, testutil.FakeClient(t))
		e.create(map[string]string{"a": "1"})
		e.edit(func(cm *corev1.ConfigMap) {
			cm.Data["a"] = "2"
			cm.BinaryData = map[string][]byte{"hidden": {1}}
		})
		return e
	}
	update := ConfigMapUpdateRequest{Data: map[string]string{"a": "3"}}

	t.Run("refused", func(t *testing.T) {
		e := drifted(t)
		w := e.PutJSON(itemPath, update)
		require.Equal(t, http.StatusConflict, w.Code)
		assert.Contains(t, w.Body.String(), apierrors.CodeDriftDetected)
		assert.Len(t, trackingtest.Drift(t, w).Fields, 2)
		assert.Equal(t, "2", e.live().Data["a"])
	})
	t.Run("accept", func(t *testing.T) {
		e := drifted(t)
		require.Equal(t, http.StatusOK, e.PutJSON(itemPath+"?onDrift=accept", update).Code)
		assert.Equal(t, update.Data, e.live().Data)
		assert.NotEmpty(t, e.live().BinaryData)
		assert.Equal(t, drift.StateInSync, e.drift().State)
	})
	t.Run("revert", func(t *testing.T) {
		e := drifted(t)
		require.Equal(t, http.StatusOK, e.PutJSON(itemPath+"?onDrift=revert", update).Code)
		assert.Equal(t, update.Data, e.live().Data)
		assert.Empty(t, e.live().BinaryData)
		assert.Equal(t, drift.StateInSync, e.drift().State)
	})
	t.Run("revert with unchanged data still discards the rest", func(t *testing.T) {
		e := drifted(t)
		w := e.PutJSON(itemPath+"?onDrift=revert", ConfigMapUpdateRequest{Data: map[string]string{"a": "1"}})
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, e.live().BinaryData)
	})
	t.Run("invalid value", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, drifted(t).PutJSON(itemPath+"?onDrift=force", update).Code)
	})
	t.Run("no drift", func(t *testing.T) {
		e := newDriftEngine(t, testutil.FakeClient(t))
		e.create(map[string]string{"a": "1"})
		require.Equal(t, http.StatusOK, e.PutJSON(itemPath, update).Code)
		assert.Equal(t, drift.StateInSync, e.drift().State)
	})
}

func TestDeleteForgetsTheObject(t *testing.T) {
	e := newDriftEngine(t, testutil.FakeClient(t))
	e.create(map[string]string{"a": "1"})
	require.Equal(t, http.StatusNoContent, e.Delete(itemPath).Code)
	assert.Zero(t, e.store.Len())
}

func TestStoreOutage(t *testing.T) {
	outage := errors.New("mongodb://user:hunter2@host is down")
	e := newDriftEngine(t, testutil.FakeClient(t))
	e.create(map[string]string{"a": "1"})
	e.store.Err = outage

	t.Run("reads are served", func(t *testing.T) {
		w := e.Get(itemPath)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, drift.StateUnavailable, trackingtest.Drift(t, w).State)
		var list ConfigMapListResponse
		require.NoError(t, json.Unmarshal(e.Get(listPath).Body.Bytes(), &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, drift.StateUnavailable, list.Items[0].Drift.State)
	})
	t.Run("requests about the desired state are refused", func(t *testing.T) {
		for name, w := range map[string]*httptest.ResponseRecorder{
			"revert":         e.PostJSON(itemPath+"/drift/revert", drift.ActionRequest{Generation: 1}),
			"accept":         e.PostJSON(itemPath+"/drift/accept", drift.ActionRequest{Generation: 1}),
			"onDrift=revert": e.PutJSON(itemPath+"?onDrift=revert", ConfigMapUpdateRequest{Data: map[string]string{"a": "9"}}),
		} {
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, name)
			assert.NotContains(t, w.Body.String(), "hunter2")
		}
		assert.Equal(t, "1", e.live().Data["a"])
	})
	t.Run("writes work without a backup", func(t *testing.T) {
		w := e.PostJSON(listPath, ConfigMapRequest{Name: "other"})
		require.Equal(t, http.StatusCreated, w.Code)
		assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed)
		for _, query := range []string{"", "?onDrift=accept"} {
			w = e.PutJSON(itemPath+query, ConfigMapUpdateRequest{Data: map[string]string{"a": "2"}})
			require.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed)
			assert.NotContains(t, w.Body.String(), "hunter2")
		}
		assert.Equal(t, "2", e.live().Data["a"])
	})
	t.Run("the next read shows what was not backed up", func(t *testing.T) {
		e.store.Err = nil
		d := trackingtest.Drift(t, e.Get(itemPath))
		assert.Equal(t, drift.StateChanged, d.State)
		assert.Equal(t, []drift.Field{{Path: "/data/a", Desired: "1", Live: "2"}}, d.Fields)
		e.store.Err = outage
		w := e.Delete(itemPath)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed)
		e.store.Err = nil
		assert.Equal(t, drift.StateMissing, trackingtest.Drift(t, e.Get(itemPath)).State)
	})
}

// The store fails when the update begins and works again when it ends. Drift was
// not checked, so recording the result would hide the changes made by hand.
func TestUncheckedUpdateIsNotRecorded(t *testing.T) {
	var e *driftEngine
	e = newDriftEngine(t, testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			e.store.Err = nil
			return c.Update(ctx, obj, opts...)
		},
	}))
	e.create(map[string]string{"a": "1"})
	e.edit(func(cm *corev1.ConfigMap) { cm.Immutable = ptr.To(true) })
	require.Equal(t, drift.StateChanged, e.drift().State)

	e.store.Err = errors.New("down")
	w := e.PutJSON(itemPath, ConfigMapUpdateRequest{Data: map[string]string{"a": "2"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed)
	require.NoError(t, e.store.Err)
	fields := e.drift().Fields
	paths := make([]string, 0, len(fields))
	for _, field := range fields {
		paths = append(paths, field.Path)
	}
	assert.Equal(t, []string{"/data/a", "/immutable"}, paths)
}

func TestUnreadableRecordDoesNotBlockUpdates(t *testing.T) {
	e := newDriftEngine(t, testutil.FakeClient(t))
	e.create(map[string]string{"a": "1"})
	record, err := e.store.Get(t.Context(), settingsRef)
	require.NoError(t, err)
	record.Desired.JSON = "not json"
	expected := record.Generation
	record.Generation++
	require.NoError(t, e.store.Save(t.Context(), record, expected))

	w := e.PutJSON(itemPath, ConfigMapUpdateRequest{Data: map[string]string{"a": "2"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed)
	assert.Equal(t, "2", e.live().Data["a"])
	// It may be readable again once its key is back, so it is kept.
	kept, err := e.store.Get(t.Context(), settingsRef)
	require.NoError(t, err)
	assert.Equal(t, "not json", kept.Desired.JSON)
}

func TestFailedSaveIsReported(t *testing.T) {
	e := newDriftEngine(t, testutil.FakeClient(t))
	e.create(map[string]string{"a": "1"})
	e.store.SaveErr = errors.New("disk full")

	w := e.PutJSON(itemPath, ConfigMapUpdateRequest{Data: map[string]string{"a": "2"}})
	require.Equal(t, http.StatusOK, w.Code)
	var resp ConfigMapResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Warnings, 1)
	assert.Equal(t, apierrors.CodeDesiredStateFailed, resp.Warnings[0].Code)
	assert.NotContains(t, w.Body.String(), "disk full")
	assert.Equal(t, "2", e.live().Data["a"])
	assert.Equal(t, drift.StateChanged, e.drift().State, "the next read shows it")

	w = e.Delete(itemPath)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed)
}

func TestRefusedClusterWriteIsNotRecorded(t *testing.T) {
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("denied")
		},
	})
	e := newDriftEngine(t, k8sClient)
	w := e.PostJSON(listPath, ConfigMapRequest{Name: "settings"})
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Zero(t, e.store.Len())
}

func TestSaveSurvivesAClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			err := c.Create(ctx, obj, opts...)
			cancel() // the client goes away right after the cluster write
			return err
		},
	})
	e := newDriftEngine(t, k8sClient)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, listPath, testutil.JSONBody(t, ConfigMapRequest{Name: "settings"}))
	e.Engine.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, 1, e.store.Len())
}

func TestConcurrentSaveRecordsTheLiveObject(t *testing.T) {
	e := newDriftEngine(t, testutil.FakeClient(t))
	e.create(map[string]string{"a": "1"})
	// Another request updates the object and records it between this request's
	// cluster write and its save.
	e.store.BeforeSave = func() {
		e.store.BeforeSave = nil
		e.edit(func(cm *corev1.ConfigMap) { cm.Data["a"] = "other" })
		record, err := e.store.Get(t.Context(), settingsRef)
		require.NoError(t, err)
		record.Generation++
		require.NoError(t, e.store.Save(t.Context(), record, record.Generation-1))
	}
	require.Equal(t, http.StatusOK, e.PutJSON(itemPath, ConfigMapUpdateRequest{Data: map[string]string{"a": "mine"}}).Code)
	assert.Equal(t, "other", e.live().Data["a"])
	assert.Equal(t, drift.StateInSync, e.drift().State)
}

func TestRevertRestartsBoundCapps(t *testing.T) {
	capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{
		Name: "app", Namespace: "team-a",
		Annotations: map[string]string{consts.BoundResourceRolloutAnnotation: consts.BoundResourceRolloutEnabled},
	}}
	capp.Spec.ConfigurationSpec.Template.Spec.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}},
	}}}
	e := newDriftEngine(t, testutil.FakeClient(t, capp))
	e.create(map[string]string{"a": "1"})
	e.edit(func(cm *corev1.ConfigMap) { cm.Data["a"] = "2" })
	require.Equal(t, http.StatusOK, e.PostJSON(itemPath+"/drift/revert", trackingtest.Action(e.drift())).Code)

	require.NoError(t, e.k8s.Get(t.Context(), client.ObjectKeyFromObject(capp), capp))
	assert.Equal(t, "ConfigMap/settings", capp.Annotations[consts.RestartReasonAnnotation])
}

func TestDisabledTracking(t *testing.T) {
	e := trackingtest.Engine(t, testutil.FakeClient(t), New())
	require.Equal(t, http.StatusCreated, e.PostJSON(listPath, ConfigMapRequest{Name: "settings"}).Code)
	w := e.Get(itemPath)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "drift")
	require.Equal(t, http.StatusNotImplemented, e.PostJSON(itemPath+"/drift/accept", drift.ActionRequest{}).Code)
	require.Equal(t, http.StatusNoContent, e.Delete(itemPath).Code)
}

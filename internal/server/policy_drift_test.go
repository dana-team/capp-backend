package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	requestpolicy "github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/internal/resources/tracking/trackingtest"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func driftRouter(t *testing.T, target requestpolicy.Target, user string, allowed []string, tracker *tracking.Tracker) *gin.Engine {
	t.Helper()
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1", AllowedNamespaces: allowed}}
	cc.SetHealthy(true)
	r := policyRouter(t, policyIdentity(user), policyManager(t, cc), target, zap.NewNop())
	group := r.Group("/api/v1/clusters/:cluster")
	configmaps.New().WithTracker(tracker).RegisterRoutes(group)
	tracking.NewAdopter(tracker, tracking.AdoptKind{
		Kind: desiredstate.KindConfigMap, NewList: func() client.ObjectList { return &corev1.ConfigMapList{} },
		Options: []client.ListOption{client.MatchingLabels{consts.ManagedLabelKey: consts.ManagedLabelValue}},
	}).RegisterRoutes(group)
	return r
}

func TestBackendPolicyMissingObjects(t *testing.T) {
	store, observations, policy := policyStore(t, nil)
	addAggregateNamespace(t, store, observations, policy, "team-b")
	addAggregateNamespace(t, store, observations, policy, "team-c")
	tracker, records := trackingtest.New(t)
	for _, namespace := range []string{"team-a", "team-b", "team-c", "team-d"} {
		ref := desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindConfigMap, Namespace: namespace, Name: namespace + "-gone"}
		require.NoError(t, records.Save(t.Context(), &desiredstate.Record{
			ID: ref.ID(), Cluster: ref.Cluster, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name,
			Desired: desiredstate.Desired{JSON: "{}"}, Generation: 1,
		}, 0))
	}
	target := requestpolicy.Target{Store: store, ExecutionClient: aggregateClient(t, interceptor.Funcs{})}

	// team-c is outside the cluster's boundary and team-d is not assigned.
	r := driftRouter(t, target, "alice", []string{"team-a", "team-b", "team-d"}, tracker)
	w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/configmaps", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var list configmaps.ConfigMapListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	names := make([]string, 0, len(list.Missing))
	for _, missing := range list.Missing {
		names = append(names, missing.Name)
	}
	require.Equal(t, []string{"team-a-gone", "team-b-gone"}, names)

	r = driftRouter(t, target, "mallory", nil, tracker)
	w = policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/configmaps", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), "gone")
}

func TestBackendPolicyDriftRoles(t *testing.T) {
	const item = "/api/v1/clusters/c1/namespaces/team-a/configmaps/settings"
	setup := func(t *testing.T) (func(user, method, path, body string) int, string) {
		store, _, _ := policyStore(t, nil)
		tracker, _ := trackingtest.New(t)
		execution := aggregateClient(t, interceptor.Funcs{}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "settings", Namespace: "team-a", Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue},
		}, Data: map[string]string{"a": "1"}})
		target := requestpolicy.Target{Store: store, ExecutionClient: execution}
		send := func(user, method, path, body string) int {
			return policyRequest(driftRouter(t, target, user, nil, tracker), method, path, body).Code
		}
		require.Equal(t, http.StatusOK, send("alice", http.MethodPost, "/api/v1/clusters/c1/namespaces/team-a/drift/adopt", ""))
		cm := &corev1.ConfigMap{}
		require.NoError(t, execution.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "settings"}, cm))
		cm.Data["a"] = "2"
		require.NoError(t, execution.Update(t.Context(), cm))
		w := policyRequest(driftRouter(t, target, "alice", nil, tracker), http.MethodGet, item, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		current := trackingtest.Drift(t, w)
		require.Equal(t, drift.StateChanged, current.State)
		body, err := json.Marshal(trackingtest.Action(current))
		require.NoError(t, err)
		return send, string(body)
	}

	send, body := setup(t)
	require.Equal(t, http.StatusForbidden, send("mallory", http.MethodPost, item+"/drift/accept", body))
	require.Equal(t, http.StatusForbidden, send("alice", http.MethodPost, item+"/drift/revert", body), "a viewer cannot change the cluster")
	require.Equal(t, http.StatusOK, send("alice", http.MethodPost, item+"/drift/accept", body), "accepting changes only the store")

	send, body = setup(t)
	require.Equal(t, http.StatusOK, send("editor", http.MethodPost, item+"/drift/revert", body))
}

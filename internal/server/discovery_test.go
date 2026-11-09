package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListClusters(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	otherStore, _, _ := policyStore(t, func(policy *corev1.ConfigMap, _ *corev1.Namespace) {
		policy.Data["policy.json"] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"bob","role":"viewer"}]}`
	})
	unsynced, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, user       string
		groups, allowed  []string
		c1Store, c2Store *authorization.Store
		operators        []string
		missingTarget    bool
		want             []string
	}{
		{name: "ordered visible clusters", user: "alice", c2Store: store, want: []string{"c1", "c2"}},
		{name: "group grant", user: "bob", groups: []string{"developers"}, c2Store: store, want: []string{"c1", "c2"}},
		{name: "same namespace has independent cluster grants", user: "alice", c2Store: otherStore, want: []string{"c1"}},
		{name: "empty grants", user: "bob", c2Store: store, want: []string{}},
		{name: "cluster specific boundary", user: "alice", allowed: []string{"other"}, c2Store: store, want: []string{"c2"}},
		{name: "incomplete target after visible target", user: "alice", c1Store: unsynced, c2Store: store, want: []string{"c2"}},
		{name: "incomplete target before visible target", user: "alice", c2Store: unsynced, want: []string{"c1"}},
		{name: "missing target store", user: "alice", want: []string{"c1"}},
		{name: "missing target", user: "alice", missingTarget: true, want: []string{"c1"}},
		{name: "all targets unavailable", user: "alice", c1Store: unsynced, c2Store: unsynced, want: []string{}},
		{name: "operator target does not require synchronization", user: "alice", c2Store: unsynced, operators: []string{"alice"}, want: []string{"c1", "c2"}},
		{name: "operator authority is cluster specific", user: "operator", c1Store: unsynced, c2Store: unsynced, operators: []string{"operator"}, want: []string{"c2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c1Store := tc.c1Store
			if c1Store == nil {
				c1Store = store
			}
			targets := map[string]middleware.PolicyTarget{
				"c1": {Store: c1Store}, "c2": {Store: tc.c2Store, OperatorUsers: tc.operators},
			}
			if tc.missingTarget {
				delete(targets, "c2")
			}
			router := discoveryRouter(t, auth.ClusterCredential{ImpersonateUser: tc.user, ImpersonateGroups: tc.groups}, targets, tc.allowed)
			w := policyRequest(router, http.MethodGet, "/api/v1/clusters", "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var result struct {
				Items []cluster.ClusterMeta `json:"items"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.NotNil(t, result.Items)
			names := make([]string, 0, len(result.Items))
			for _, item := range result.Items {
				names = append(names, item.Name)
				require.Equal(t, item.Name == "c1", item.Healthy)
			}
			require.Equal(t, tc.want, names)
		})
	}
}

func TestGetCluster(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	unsynced, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, user, target string
		c2Store            *authorization.Store
		operators, allowed []string
		status             int
	}{
		{name: "eligible unhealthy metadata", user: "alice", target: "c2", c2Store: store, status: http.StatusOK},
		{name: "invisible cluster", user: "bob", target: "c2", c2Store: store, status: http.StatusNotFound},
		{name: "excluded grant", user: "alice", target: "c1", c2Store: store, allowed: []string{"other"}, status: http.StatusNotFound},
		{name: "unknown cluster", user: "alice", target: "unknown", c2Store: store, status: http.StatusNotFound},
		{name: "incomplete target", user: "alice", target: "c2", c2Store: unsynced, status: http.StatusServiceUnavailable},
		{name: "unrelated incomplete target", user: "alice", target: "c1", c2Store: unsynced, status: http.StatusOK},
		{name: "operator without observations", user: "operator", target: "c2", c2Store: unsynced, operators: []string{"operator"}, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := discoveryRouter(t, auth.ClusterCredential{ImpersonateUser: tc.user}, map[string]middleware.PolicyTarget{
				"c1": {Store: store}, "c2": {Store: tc.c2Store, OperatorUsers: tc.operators},
			}, tc.allowed)
			w := policyRequest(router, http.MethodGet, "/api/v1/clusters/"+tc.target, "")
			require.Equal(t, tc.status, w.Code, w.Body.String())
			switch tc.status {
			case http.StatusOK:
				var result cluster.ClusterMeta
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
				require.Equal(t, tc.target, result.Name)
				require.Equal(t, tc.target == "c1", result.Healthy)
			case http.StatusNotFound:
				requirePolicyError(t, w, apierrors.CodeClusterNotFound, "")
			case http.StatusServiceUnavailable:
				requirePolicyError(t, w, apierrors.CodeInternal, "POLICY_UNAVAILABLE")
			}
		})
	}
}

func TestListClustersLegacy(t *testing.T) {
	items := []cluster.ClusterMeta{{Name: "zeta", Healthy: false}, {Name: "alpha", Healthy: true}}
	mgr := &testutil.MockClusterManager{ListFn: func() []cluster.ClusterMeta { return items }}
	router := gin.New()
	router.GET("/api/v1/clusters", listClusters(mgr))
	router.GET("/api/v1/clusters/:cluster", getCluster(mgr))
	w := policyRequest(router, http.MethodGet, "/api/v1/clusters", "")
	require.Equal(t, http.StatusOK, w.Code)
	var result struct {
		Items []cluster.ClusterMeta `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Equal(t, items, result.Items)
	w = policyRequest(router, http.MethodGet, "/api/v1/clusters/zeta", "")
	require.Equal(t, http.StatusOK, w.Code)
	var detail cluster.ClusterMeta
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &detail))
	require.Equal(t, items[0], detail)
}

func discoveryRouter(t *testing.T, credential auth.ClusterCredential, targets map[string]middleware.PolicyTarget, allowed []string) *gin.Engine {
	t.Helper()
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1", AllowedNamespaces: allowed}}
	cc.SetHealthy(true)
	other := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c2"}}
	mgr := policyManager(t, cc)
	mgr.GetFn = func(name string) (*cluster.ClusterClient, error) {
		switch name {
		case "c1":
			return cc, nil
		case "c2":
			return other, nil
		default:
			return nil, cluster.ErrClusterNotFound
		}
	}
	mgr.ListFn = func() []cluster.ClusterMeta {
		first := cc.Meta
		first.Healthy = cc.IsHealthy()
		return []cluster.ClusterMeta{other.Meta, first}
	}
	for name, target := range targets {
		target.ExecutionClient = &clientThatMustNotRun{t: t}
		targets[name] = target
	}
	rules, err := middleware.NewPolicyRoutes("")
	require.NoError(t, err)
	identity := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) { return credential, nil }}
	router := gin.New()
	router.Use(middleware.BackendPolicy(identity, mgr, rules, targets, zap.NewNop()))
	router.GET("/api/v1/clusters", listClusters(mgr))
	router.GET("/api/v1/clusters/:cluster", getCluster(mgr))
	return router
}

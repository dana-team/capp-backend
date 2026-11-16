package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	requestpolicy "github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	authv1 "k8s.io/api/authentication/v1"
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
			targets := map[string]requestpolicy.Target{
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
			router := discoveryRouter(t, auth.ClusterCredential{ImpersonateUser: tc.user}, map[string]requestpolicy.Target{
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

func TestListClustersKubernetes(t *testing.T) {
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

func discoveryRouter(t *testing.T, credential auth.ClusterCredential, targets map[string]requestpolicy.Target, allowed []string) *gin.Engine {
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
	rules, err := requestpolicy.NewRoutes("")
	require.NoError(t, err)
	identity := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) { return credential, nil }}
	router := gin.New()
	router.Use(requestpolicy.New(identity, credential.AuthProvider, mgr, rules, targets, zap.NewNop()))
	router.GET("/api/v1/clusters", listClusters(mgr))
	router.GET("/api/v1/clusters/:cluster", getCluster(mgr))
	return router
}

func TestListClustersVerifiedIdentities(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	unsynced, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		first, second *authv1.UserInfo
		outage        int64
		firstOutage   int64
		unsynced      bool
		want          []string
		status        int
	}{
		{name: "separate identities", first: &authv1.UserInfo{Username: "alice"}, second: &authv1.UserInfo{Username: "bob"}, want: []string{"c1"}, status: http.StatusOK},
		{name: "partial rejection", first: &authv1.UserInfo{Username: "alice"}, want: []string{"c1"}, status: http.StatusOK},
		{name: "all rejected", status: http.StatusUnauthorized},
		{name: "accepted without grants", first: &authv1.UserInfo{Username: "bob"}, want: []string{}, status: http.StatusOK},
		{name: "outage with accepted token", first: &authv1.UserInfo{Username: "alice"}, outage: http.StatusForbidden, want: []string{"c1"}, status: http.StatusOK},
		{name: "first reviewer outage with accepted token", second: &authv1.UserInfo{Username: "alice"}, firstOutage: http.StatusServiceUnavailable, want: []string{"c2"}, status: http.StatusOK},
		{name: "outage with accepted but invisible token", first: &authv1.UserInfo{Username: "bob"}, outage: http.StatusServiceUnavailable, want: []string{}, status: http.StatusOK},
		{name: "first reviewer outage with rejected token", firstOutage: http.StatusServiceUnavailable, status: http.StatusServiceUnavailable},
		{name: "all reviewers unavailable", firstOutage: http.StatusServiceUnavailable, outage: http.StatusServiceUnavailable, status: http.StatusServiceUnavailable},
		{name: "outage with rejected token", outage: http.StatusUnauthorized, status: http.StatusServiceUnavailable},
		{name: "unsynchronized policy is omitted", first: &authv1.UserInfo{Username: "alice"}, second: &authv1.UserInfo{Username: "alice"}, unsynced: true, want: []string{"c1"}, status: http.StatusOK},
		{name: "operator membership stays at target", first: &authv1.UserInfo{Username: "operator", Groups: []string{"operators"}}, second: &authv1.UserInfo{Username: "bob"}, unsynced: true, want: []string{"c1"}, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPolicyProvider(t, config.AuthModePassthrough, map[string]authv1.UserInfo{"c1": {}, "c2": {}}, nil)
			p.reviews["c1"].user.Store(tc.first)
			p.reviews["c2"].user.Store(tc.second)
			p.reviews["c1"].status.Store(tc.firstOutage)
			p.reviews["c2"].status.Store(tc.outage)
			secondStore := store
			if tc.unsynced {
				secondStore = unsynced
			}
			router := p.router(t, map[string]requestpolicy.Target{
				"c1": {Store: store, OperatorGroups: []string{"operators"}},
				"c2": {Store: secondStore, OperatorGroups: []string{"operators"}},
			})
			for range 2 {
				w := policyRequest(router, http.MethodGet, "/api/v1/clusters", "")
				if w.Code != tc.status {
					t.Fatalf("discovery = %d %s, want %d", w.Code, w.Body.String(), tc.status)
				}
				if tc.status == http.StatusOK {
					requireClusterNames(t, w, tc.want)
					continue
				}
				message, code := auth.ErrUnauthenticated.Error(), apierrors.CodeUnauthorized
				if tc.status == http.StatusServiceUnavailable {
					message, code = auth.ErrIdentityUnavailable.Error(), apierrors.CodeInternal
				}
				var response struct {
					Error apierrors.APIError `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Error.Code != code || response.Error.Message != message {
					t.Fatalf("error = %+v, want %s / %s", response.Error, code, message)
				}
				if strings.Contains(w.Body.String(), "items") {
					t.Fatal("failed discovery published a partial collection")
				}
			}
			for name, review := range p.reviews {
				if got := review.calls.Load(); got != 2 {
					t.Errorf("%s reviews = %d, want 2", name, got)
				}
			}
		})
	}
}

func TestListClustersReviewConcurrency(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRequest), func(t *testing.T) {
			users := make(map[string]authv1.UserInfo)
			targets := make(map[string]requestpolicy.Target)
			for i := range 9 {
				name := fmt.Sprintf("c%d", i)
				users[name] = authv1.UserInfo{Username: "operator"}
				targets[name] = requestpolicy.Target{OperatorUsers: []string{"operator"}}
			}
			p := newPolicyProvider(t, config.AuthModePassthrough, users, nil)
			var active, peak atomic.Int64
			// Each configured reviewer can announce its entry once.
			entered := make(chan struct{}, len(users))
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			for name, review := range p.reviews {
				if name == "c0" {
					continue
				}
				review.before = func(ctx context.Context) {
					current := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
					}
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
			router := p.router(t, targets)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/clusters", nil)
			req.Header.Set("Authorization", "Bearer caller-token")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); router.ServeHTTP(w, req) }()
			for range 4 {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("four concurrent reviews did not start")
				}
			}
			if cancelRequest {
				cancel()
			} else {
				releaseOnce.Do(func() { close(release) })
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("discovery did not join review workers")
			}
			if got := peak.Load(); got != 4 {
				t.Errorf("peak concurrent reviews = %d, want 4", got)
			}
			if cancelRequest {
				if w.Code != middleware.StatusClientClosedRequest || w.Body.Len() != 0 {
					t.Fatalf("cancelled discovery = %d %s", w.Code, w.Body.String())
				}
			} else {
				if w.Code != http.StatusOK {
					t.Fatalf("discovery = %d %s", w.Code, w.Body.String())
				}
				for name, review := range p.reviews {
					if got := review.calls.Load(); got != 1 {
						t.Errorf("%s reviews = %d, want 1", name, got)
					}
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for active.Load() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if active.Load() != 0 {
				t.Fatal("review handlers remained active after cancellation")
			}
		})
	}
}

func TestListClustersOpenShiftHomeIdentity(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejected=%t", rejected), func(t *testing.T) {
			store, _, _ := policyStore(t, nil)
			provider := newPolicyProvider(t, config.AuthModeOpenShift, map[string]authv1.UserInfo{
				"c1": {Username: "alice"}, "c2": {Username: "unassigned-target-user"},
			}, nil)
			if rejected {
				provider.reviews["c1"].user.Store(nil)
			}
			router := provider.router(t, map[string]requestpolicy.Target{"c1": {Store: store}, "c2": {Store: store}})
			response := policyRequest(router, http.MethodGet, "/api/v1/clusters", "")
			if rejected {
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("rejected discovery = %d, want %d", response.Code, http.StatusUnauthorized)
				}
			} else {
				requireClusterNames(t, response, []string{"c1", "c2"})
			}
			if got := provider.reviews["c1"].calls.Load(); got != 1 {
				t.Errorf("OpenShift discovery home reviews = %d, want 1", got)
			}
			if got := provider.reviews["c2"].calls.Load(); got != 0 {
				t.Errorf("OpenShift discovery target reviews = %d, want 0", got)
			}
		})
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestListClusters(t *testing.T) {
	for _, tc := range []struct {
		name       string
		user       string
		groups     []string
		empty      bool
		invalidate string
		want       bool
		status     int
	}{
		{name: "resource-only direct membership on unmanaged namespace", user: "alice", want: true},
		{name: "operator user binding", user: "operator", want: true},
		{name: "verified group binding", user: "bob", groups: []string{"editors"}, want: true},
		{name: "global grant without membership", user: "global", want: true},
		{name: "empty role", user: "empty"},
		{name: "cluster-only permission does not imply namespace access", user: "creator"},
		{name: "protected namespace", user: "protected"},
		{name: "policy namespace", user: "policies"},
		{name: "configuration-excluded namespace", user: "excluded"},
		{name: "no grants", user: "nobody"},
		{name: "empty namespace collection", user: "global", empty: true},
		{name: "cluster grant with unavailable namespaces", user: "admin", invalidate: "namespaces", want: true},
		{name: "required namespace generation unavailable", user: "alice", invalidate: "namespaces", status: http.StatusServiceUnavailable},
		{name: "required policy generation unavailable", user: "admin", invalidate: "policy", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiscoveryFixture(t, tc.empty)
				if tc.user == "global" {
					f.clusters[0].AllowedNamespaces = []string{"other"}
				}
				snapshot, err := f.store.Snapshot(auth.ClusterCredential{ImpersonateUser: tc.user, ImpersonateGroups: tc.groups})
				require.NoError(t, err)
				switch tc.invalidate {
				case "namespaces":
					f.store.InvalidateNamespaces()
				case "policy":
					f.store.Invalidate()
				}
				state := f.state(snapshot)
				response := f.serve(state, "")
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				require.Equal(t, status, response.Code, response.Body.String())
				if status != http.StatusOK {
					assert.Contains(t, response.Body.String(), `"reason":"POLICY_UNAVAILABLE"`)
					assert.NotContains(t, response.Body.String(), `"items"`)
					return
				}
				var result struct {
					Items []cluster.ClusterMeta `json:"items"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
				want := []cluster.ClusterMeta{}
				if tc.want {
					want = []cluster.ClusterMeta{f.clusters[0]}
					want[0].AllowedNamespaces = nil
				}
				assert.Equal(t, want, result.Items)
			})
		})
	}

	t.Run("later target failure prevents partial list", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiscoveryFixture(t, false)
			snapshot, err := f.store.Snapshot(auth.ClusterCredential{ImpersonateUser: "admin"})
			require.NoError(t, err)
			f.clusters = append(f.clusters, cluster.ClusterMeta{Name: "broken"})
			state := f.state(snapshot)
			state.Targets["broken"] = &middleware.AuthorizationTarget{Snapshot: &authorization.Snapshot{}}
			response := f.serve(state, "")
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.NotContains(t, response.Body.String(), `"items"`)
		})
	})
	t.Run("later namespace evaluation error prevents partial list", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiscoveryFixture(t, false)
			var namespace corev1.Namespace
			require.NoError(t, f.base.Get(t.Context(), client.ObjectKey{Name: "excluded"}, &namespace))
			require.NoError(t, f.base.Delete(t.Context(), &namespace))
			namespace.Name, namespace.ResourceVersion = "z-mismatched", ""
			require.NoError(t, f.base.Create(t.Context(), &namespace))
			f.namespaceWatch.Add(&namespace)
			synctest.Wait()
			f.clusters[0].AllowedNamespaces = append(f.clusters[0].AllowedNamespaces, namespace.Name)
			snapshot, err := f.store.Snapshot(auth.ClusterCredential{ImpersonateUser: "alice"})
			require.NoError(t, err)
			response := f.serve(f.state(snapshot), "")
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.NotContains(t, response.Body.String(), `"items"`)
		})
	})
	t.Run("namespace recreation converges and retires old snapshots", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiscoveryFixture(t, false)
			credential := auth.ClusterCredential{ImpersonateUser: "alice"}
			snapshot, err := f.store.Snapshot(credential)
			require.NoError(t, err)
			var namespace corev1.Namespace
			require.NoError(t, f.base.Get(t.Context(), client.ObjectKey{Name: "team"}, &namespace))
			require.NoError(t, f.base.Delete(t.Context(), &namespace))
			namespace.UID, namespace.ResourceVersion = "replacement", ""
			require.NoError(t, f.base.Create(t.Context(), &namespace))
			time.Sleep(10 * time.Second)
			synctest.Wait()
			assert.Equal(t, http.StatusOK, f.serve(f.state(snapshot), "prod").Code)
			f.namespaceWatch.Modify(&namespace)
			synctest.Wait()
			current, err := f.store.Snapshot(credential)
			require.NoError(t, err)
			assert.Equal(t, http.StatusForbidden, f.serve(f.state(current), "prod").Code)
			assert.Equal(t, http.StatusServiceUnavailable, f.serve(f.state(snapshot), "prod").Code)
		})
	})
}

func TestGetCluster(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryFixture(t, false)
		snapshot, err := f.store.Snapshot(auth.ClusterCredential{ImpersonateUser: "nobody"})
		require.NoError(t, err)
		response := f.serve(f.state(snapshot), "prod")
		assert.Equal(t, http.StatusForbidden, response.Code)
		assert.Contains(t, response.Body.String(), `"reason":"POLICY_DENIED"`)
		response = f.serve(f.state(snapshot), "unknown")
		assert.Equal(t, http.StatusNotFound, response.Code)
		assert.JSONEq(t, `{"error":{"code":"CLUSTER_NOT_FOUND","message":"cluster \"unknown\" is not configured","status":404}}`, response.Body.String())
		// Without prepared application authorization, preserve legacy discovery.
		response = f.serve(nil, "prod")
		require.Equal(t, http.StatusOK, response.Code)
		var meta cluster.ClusterMeta
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &meta))
		want := f.clusters[0]
		want.AllowedNamespaces = nil
		assert.Equal(t, want, meta)
		response = f.serve(nil, "")
		require.Equal(t, http.StatusOK, response.Code)
		assert.Contains(t, response.Body.String(), `"name":"prod"`)
	})
}

type discoveryFixture struct {
	store          *authorization.Store
	base           client.Client
	config         config.ClusterAuthorizationConfig
	clusters       []cluster.ClusterMeta
	namespaceWatch *watch.RaceFreeFakeWatcher
	request        context.Context
}

func newDiscoveryFixture(t *testing.T, empty bool) *discoveryFixture {
	t.Helper()
	f := &discoveryFixture{
		request:  t.Context(),
		config:   config.ClusterAuthorizationConfig{PolicyNamespace: "policies", ProtectedNamespaces: []string{"protected"}},
		clusters: []cluster.ClusterMeta{{Name: "prod", AllowedNamespaces: []string{"team", "other", "protected", "policies"}}},
	}
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "capp-policy", Namespace: "policies", Labels: map[string]string{consts.CappAPIGroup + "/authorization": "true"},
	}, Data: map[string]string{"policy.json": `{"version":1,"roles":{"capp-user":[{"resource":"capps","actions":["get"]}],"empty":[],"discover":[{"resource":"clusters","actions":["get"]}],"creator":[{"resource":"namespaces","actions":["create"]}]},"bindings":[{"kind":"User","name":"admin","role":"discover","scope":"cluster"},{"kind":"User","name":"creator","role":"creator","scope":"cluster"},{"kind":"User","name":"global","role":"capp-user","scope":"allNamespaces"}]}`}}
	objects := []client.Object{policy}
	if !empty {
		for _, name := range []string{"team", "other", "protected", "excluded", "policies"} {
			uid := types.UID("uid-" + name)
			objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}})
			if name == "other" {
				continue
			}
			user := name
			bindings := []map[string]string{}
			if name == "team" {
				user = "alice"
				bindings = []map[string]string{
					{"kind": "User", "name": "operator", "role": "capp-user"},
					{"kind": "Group", "name": "editors", "role": "capp-user"},
					{"kind": "User", "name": "empty", "role": "empty"},
				}
			}
			payload, err := json.Marshal(map[string]any{"version": 1, "namespace": map[string]string{"name": name, "uid": string(uid)}, "users": []string{user}, "bindings": bindings})
			require.NoError(t, err)
			objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "capp-members-" + string(uid), Namespace: "policies", Labels: policy.Labels,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: name, UID: uid}},
			}, Data: map[string]string{"policy.json": string(payload)}})
		}
	}
	f.base = testutil.FakeClient(t, objects...)
	base, ok := f.base.(client.WithWatch)
	require.True(t, ok)
	target := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if ctx == t.Context() {
				t.Fatal("discovery performed a request-time LIST")
			}
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			list.SetResourceVersion("1")
			return nil
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("discovery performed a GET")
			return nil
		},
		Watch: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
			stream := watch.NewRaceFreeFake()
			if _, ok := list.(*corev1.NamespaceList); ok {
				f.namespaceWatch = stream
			}
			return stream, nil
		},
	})
	var err error
	f.store, err = authorization.NewStore(target, f.config, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(f.store.Stop)
	require.NoError(t, f.store.Start(t.Context()))
	synctest.Wait()
	time.Sleep(time.Second)
	synctest.Wait()
	return f
}

func (f *discoveryFixture) state(snapshot *authorization.Snapshot) *middleware.AuthorizationState {
	return &middleware.AuthorizationState{Targets: map[string]*middleware.AuthorizationTarget{
		"prod": {Snapshot: snapshot, Config: f.config},
	}}
}

func (f *discoveryFixture) serve(state *middleware.AuthorizationState, name string) *httptest.ResponseRecorder {
	router := gin.New()
	if state != nil {
		router.Use(func(c *gin.Context) { c.Set(string(middleware.AuthorizationKey), state) })
	}
	mgr := &testutil.MockClusterManager{ListFn: func() []cluster.ClusterMeta { return f.clusters }}
	router.GET("/clusters", listClusters(mgr))
	router.GET("/clusters/:cluster", getCluster(mgr))
	path := "/clusters"
	if name != "" {
		path += "/" + name
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil).WithContext(f.request))
	return response
}

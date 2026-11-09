package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	requestpolicy "github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPrepareMigration(t *testing.T) {
	sourceStore, _, _ := policyStore(t, nil)
	destinationStore, observations, cm := policyStore(t, nil)
	unsyncedStore, err := authorization.NewStore(observations, authorization.StoreOptions{PolicyNamespace: cm.Namespace})
	require.NoError(t, err)
	viewerStore, _, _ := policyStore(t, func(cm *corev1.ConfigMap, _ *corev1.Namespace) {
		cm.Data["policy.json"] = aggregatePolicyJSON(t, "team-a", []authorization.Binding{
			{Kind: rbacv1.UserKind, Name: "editor", Role: authorization.RoleViewer},
		})
	})
	pendingStore, _, _ := policyStore(t, func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Annotations = nil })
	for _, tc := range []struct {
		name                              string
		source, destination               *authorization.Store
		sourceAllowed, destinationAllowed []string
		targetCluster, targetNamespace    string
		unhealthy                         bool
		status                            int
	}{
		{name: "source viewer", source: viewerStore, destination: destinationStore, status: http.StatusForbidden},
		{name: "destination viewer", source: sourceStore, destination: viewerStore, status: http.StatusForbidden},
		{name: "unhealthy destination viewer", source: sourceStore, destination: viewerStore, unhealthy: true, status: http.StatusForbidden},
		{name: "source outside boundary", source: sourceStore, destination: destinationStore, sourceAllowed: []string{"other"}, status: http.StatusForbidden},
		{name: "destination outside boundary", source: sourceStore, destination: destinationStore, destinationAllowed: []string{"other"}, status: http.StatusForbidden},
		{name: "unhealthy destination outside boundary", source: sourceStore, destination: destinationStore, destinationAllowed: []string{"other"}, unhealthy: true, status: http.StatusForbidden},
		{name: "destination pending association", source: sourceStore, destination: pendingStore, status: http.StatusForbidden},
		{name: "destination observations absent", source: sourceStore, status: http.StatusForbidden},
		{name: "destination observations unsynchronized", source: sourceStore, destination: unsyncedStore, status: http.StatusForbidden},
		{name: "destination protected namespace", source: sourceStore, destination: destinationStore, targetNamespace: "capp-system", status: http.StatusForbidden},
		{name: "destination unassigned", source: sourceStore, destination: destinationStore, targetNamespace: "other", status: http.StatusForbidden},
		{name: "unhealthy destination unassigned", source: sourceStore, destination: destinationStore, targetNamespace: "other", unhealthy: true, status: http.StatusForbidden},
		{name: "same cluster unassigned destination", source: sourceStore, destination: destinationStore, targetCluster: "c1", targetNamespace: "other", status: http.StatusForbidden},
		{name: "unknown destination", source: sourceStore, targetCluster: "missing", status: http.StatusForbidden},
		{name: "unhealthy destination", source: sourceStore, destination: destinationStore, unhealthy: true, status: http.StatusServiceUnavailable},
		{name: "malformed namespace", source: sourceStore, destination: destinationStore, targetNamespace: "BAD", status: http.StatusBadRequest},
		{name: "identical endpoints", source: sourceStore, destination: destinationStore, targetCluster: "c1", status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution := aggregateClient(t, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					t.Fatal("denied migration read storage")
					return nil
				},
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("denied migration listed storage")
					return nil
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					t.Fatal("denied migration wrote storage")
					return nil
				},
			})
			targets := map[string]requestpolicy.Target{"c1": {Store: tc.source, ExecutionClient: execution}, "c2": {Store: tc.destination, ExecutionClient: execution}}
			r := migrationRouter(t, targets, tc.sourceAllowed, tc.destinationAllowed, tc.unhealthy, zap.NewNop())
			targetCluster, targetNamespace := tc.targetCluster, tc.targetNamespace
			if targetCluster == "" {
				targetCluster = "c2"
			}
			if targetNamespace == "" {
				targetNamespace = "team-a"
			}
			body, err := json.Marshal(capps.MigrateRequest{TargetCluster: targetCluster, TargetNamespace: targetNamespace})
			require.NoError(t, err)
			w := policyRequest(r, http.MethodPost, "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", string(body))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusForbidden {
				requirePolicyError(t, w, apierrors.CodeForbidden, apierrors.ReasonPolicyDenied)
			}
		})
	}
	t.Run("wrong target decision cannot be reused", func(t *testing.T) {
		cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1"}}
		cc.SetHealthy(true)
		r := policyRouter(t, policyIdentity("editor"), policyManager(t, cc), requestpolicy.Target{Store: sourceStore}, zap.NewNop())
		r.POST("/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate", func(c *gin.Context) {
			c.Params = append(c.Params[:0], gin.Param{Key: "cluster", Value: "c1"}, gin.Param{Key: "namespace", Value: "other"})
			_, _, ok := requestpolicy.PrepareMigration(c, "c2", "team-a")
			require.False(t, ok)
		})
		w := policyRequest(r, http.MethodPost, "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", "")
		require.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestMigrateBackendPolicy(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	for _, tc := range []struct {
		name                                           string
		deleteSource, failCreate, failDelete, failRead bool
	}{
		{name: "copy"}, {name: "move", deleteSource: true}, {name: "destination failure cleans copied resources", failCreate: true},
		{name: "source deletion failure preserves destination", deleteSource: true, failDelete: true}, {name: "source execution denial has no fallback", failRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "secret", Namespace: "team-a", Labels: map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}}, Data: map[string][]byte{"key": []byte("source-value")}}
			sourceGets, destinationCreates := 0, 0
			source := aggregateClient(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					sourceGets++
					require.Equal(t, "team-a", key.Namespace)
					if tc.failRead {
						return k8serrors.NewForbidden(schema.GroupResource{Resource: "capps"}, key.Name, errors.New("execution denied"))
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					t.Fatal("destination write used source credentials")
					return nil
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if tc.failDelete {
						return errors.New("source delete failed")
					}
					return c.Delete(ctx, obj, opts...)
				},
			}, capp, secret)
			destination := aggregateClient(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					require.Equal(t, "team-a", key.Namespace, "namespace metadata must come from observations")
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					destinationCreates++
					if _, ok := obj.(*cappv1alpha1.Capp); ok && tc.failCreate {
						return k8serrors.NewForbidden(schema.GroupResource{Resource: "capps"}, obj.GetName(), errors.New("webhook denied"))
					}
					return c.Create(ctx, obj, opts...)
				},
			})
			core, logs := observer.New(zap.InfoLevel)
			r := migrationRouter(t, map[string]requestpolicy.Target{"c1": {Store: store, ExecutionClient: source}, "c2": {Store: store, ExecutionClient: destination}}, nil, nil, false, zap.New(core))
			body, err := json.Marshal(capps.MigrateRequest{TargetCluster: "c2", TargetNamespace: "team-a", DeleteSource: tc.deleteSource})
			require.NoError(t, err)
			w := policyRequest(r, http.MethodPost, "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", string(body))
			require.Equal(t, 1, sourceGets)
			if tc.failRead {
				require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				require.Zero(t, destinationCreates)
				return
			}
			require.Equal(t, 2, destinationCreates)
			if tc.failCreate {
				require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				require.True(t, k8serrors.IsNotFound(destination.Get(t.Context(), client.ObjectKeyFromObject(secret), &corev1.Secret{})))
				return
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var response capps.MigrateResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Equal(t, tc.deleteSource && !tc.failDelete, response.SourceDeleted)
			sourceErr := source.Get(t.Context(), client.ObjectKeyFromObject(capp), &cappv1alpha1.Capp{})
			if response.SourceDeleted {
				require.True(t, k8serrors.IsNotFound(sourceErr))
			} else {
				require.NoError(t, sourceErr)
			}
			require.Equal(t, []string{"secret"}, response.CopiedSecrets)
			var copied corev1.Secret
			require.NoError(t, destination.Get(t.Context(), client.ObjectKeyFromObject(secret), &copied))
			require.Equal(t, secret.Data, copied.Data)
			require.NoError(t, destination.Get(t.Context(), client.ObjectKeyFromObject(capp), &cappv1alpha1.Capp{}))
			decisions := logs.FilterMessage("authorization decision").All()
			require.Len(t, decisions, 2)
			require.Equal(t, "c2", decisions[0].ContextMap()["cluster"])
			require.Equal(t, "c1", decisions[1].ContextMap()["cluster"])
			for _, entry := range decisions {
				require.Equal(t, "policy-uid", entry.ContextMap()["policyUID"])
				require.Equal(t, "allowed", entry.ContextMap()["outcome"])
			}
		})
	}
}

func migrationRouter(t *testing.T, targets map[string]requestpolicy.Target, sourceAllowed, destinationAllowed []string, unhealthy bool, logger *zap.Logger) *gin.Engine {
	t.Helper()
	source := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1", AllowedNamespaces: sourceAllowed}}
	source.SetHealthy(true)
	destination := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c2", AllowedNamespaces: destinationAllowed}}
	destination.SetHealthy(!unhealthy)
	mgr := policyManager(t, source)
	mgr.GetFn = func(name string) (*cluster.ClusterClient, error) {
		switch name {
		case "c1":
			return source, nil
		case "c2":
			return destination, nil
		default:
			return nil, cluster.ErrClusterNotFound
		}
	}
	rules, err := requestpolicy.NewRoutes("")
	require.NoError(t, err)
	r := gin.New()
	r.Use(requestpolicy.New(policyIdentity("editor"), mgr, rules, targets, logger))
	capps.New(false, nil, mgr, config.CappSizes{}).RegisterRoutes(r.Group("/api/v1/clusters/:cluster"))
	return r
}

func TestPrepareMigrationNamespaceIsolation(t *testing.T) {
	store, observations, policy := policyStore(t, nil)
	ns := store.Lookup("team-a").Namespace.DeepCopy()
	ns.Name = "team-b"
	_, err := observations.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{})
	require.NoError(t, err)
	cm := policy.DeepCopy()
	cm.Name = "capp-assignment-team-b"
	cm.Data["policy.json"] = aggregatePolicyJSON(t, "team-b", []authorization.Binding{
		{Kind: rbacv1.UserKind, Name: "editor", Role: authorization.RoleEditor},
	})
	_, err = observations.CoreV1().ConfigMaps(cm.Namespace).Create(t.Context(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Lookup("team-b").Ready() }, 5*time.Second, time.Millisecond)
	var namespaces []string
	execution := aggregateClient(t, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("cross-namespace GET reached storage")
			return nil
		},
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, opts ...client.ListOption) error {
			namespaces = append(namespaces, (&client.ListOptions{}).ApplyOptions(opts).Namespace)
			return nil
		},
	})
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1"}}
	cc.SetHealthy(true)
	r := policyRouter(t, policyIdentity("editor"), policyManager(t, cc), requestpolicy.Target{Store: store, ExecutionClient: execution}, zap.NewNop())
	r.POST("/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate", func(c *gin.Context) {
		source, destination, ok := requestpolicy.PrepareMigration(c, "c1", "team-b")
		require.True(t, ok)
		require.NoError(t, source.List(c.Request.Context(), &corev1.SecretList{}))
		require.NoError(t, destination.List(c.Request.Context(), &corev1.SecretList{}))
		require.Error(t, source.Get(c.Request.Context(), client.ObjectKey{Namespace: "team-b", Name: "secret"}, &corev1.Secret{}))
		require.Error(t, destination.Get(c.Request.Context(), client.ObjectKey{Namespace: "team-a", Name: "secret"}, &corev1.Secret{}))
	})
	w := policyRequest(r, http.MethodPost, "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, []string{"team-a", "team-b"}, namespaces)
}

func TestPrepareMigrationCredentials(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	targets := make(map[string]requestpolicy.Target)
	for _, name := range []string{"c1", "c2"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+name+"-token" || r.Header.Get(transport.ImpersonateUserHeader) != "" {
				t.Errorf("%s received incorrect execution credentials", name)
			}
			w.Header().Set("Content-Type", "application/json")
			var body any
			switch r.URL.Path {
			case "/api":
				body = metav1.APIVersions{Versions: []string{"v1"}}
			case "/apis":
				body = metav1.APIGroupList{}
			case "/api/v1":
				body = metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "secrets", Kind: "Secret", Namespaced: true}}}
			case "/api/v1/namespaces/team-a/secrets/item":
				body = corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: "item", Namespace: "team-a"}, Data: map[string][]byte{"cluster": []byte(name)}}
			default:
				t.Errorf("unexpected migration request: %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Error(err)
			}
		}))
		t.Cleanup(server.Close)
		clients, err := cluster.NewSystemClients(&cluster.ClusterClient{RestConfig: &rest.Config{Host: server.URL, BearerToken: name + "-token", Impersonate: rest.ImpersonationConfig{UserName: "caller"}}, Scheme: testutil.TestScheme(t)})
		require.NoError(t, err)
		targets[name] = requestpolicy.Target{Store: store, ExecutionClient: clients.Resources}
	}
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1"}}
	cc.SetHealthy(true)
	mgr := policyManager(t, cc)
	mgr.GetFn = func(name string) (*cluster.ClusterClient, error) {
		target := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: name}}
		target.SetHealthy(true)
		return target, nil
	}
	rules, err := requestpolicy.NewRoutes("")
	require.NoError(t, err)
	r := gin.New()
	r.Use(requestpolicy.New(policyIdentity("editor"), mgr, rules, targets, zap.NewNop()))
	r.POST("/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate", func(c *gin.Context) {
		source, destination, ok := requestpolicy.PrepareMigration(c, "c2", "team-a")
		require.True(t, ok)
		for i, execution := range []client.Client{source, destination} {
			var secret corev1.Secret
			require.NoError(t, execution.Get(c.Request.Context(), client.ObjectKey{Namespace: "team-a", Name: "item"}, &secret))
			require.Equal(t, []string{"c1", "c2"}[i], string(secret.Data["cluster"]))
		}
	})
	w := policyRequest(r, http.MethodPost, "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

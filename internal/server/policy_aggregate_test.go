package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/secrets"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestListAggregate(t *testing.T) {
	for _, resource := range []string{"capps", "configmaps", "secrets"} {
		t.Run(resource, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			store, observations, policy := policyStore(t, nil)
			addAggregateNamespace(t, store, observations, policy, "team-b")
			addAggregateNamespace(t, store, observations, policy, "team-c")
			objects := make([]client.Object, 0, 17)
			for _, entry := range []struct{ namespace, name string }{
				{"team-a", "a"}, {"team-a", "z"}, {"team-b", "b"},
				{"team-c", "excluded"}, {"team-d", "unassigned"},
			} {
				metadata := metav1.ObjectMeta{Name: entry.name, Namespace: entry.namespace}
				objects = append(objects, &cappv1alpha1.Capp{ObjectMeta: metadata})
				metadata.Labels = map[string]string{consts.ManagedLabelKey: consts.ManagedLabelValue}
				objects = append(objects, &corev1.ConfigMap{ObjectMeta: metadata},
					&corev1.Secret{ObjectMeta: metadata, Data: map[string][]byte{"value": []byte(entry.name + "-payload")}})
			}
			objects = append(objects,
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: "team-a"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: "team-a"}, Data: map[string][]byte{"value": []byte("unmanaged-payload")}})
			var calls []string
			execution := aggregateClient(t, interceptor.Funcs{
				List: func(ctx context.Context, reader client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					options := (&client.ListOptions{}).ApplyOptions(opts)
					require.Contains(t, []string{"team-a", "team-b"}, options.Namespace)
					calls = append(calls, options.Namespace+":"+options.Continue)
					if err := reader.List(ctx, list, opts...); err != nil {
						return err
					}
					if options.Namespace == "team-a" {
						items, err := meta.ExtractList(list)
						require.NoError(t, err)
						require.Len(t, items, 2, "resource-specific selector must be preserved")
						if options.Continue == "" {
							require.NoError(t, meta.SetList(list, items[1:]))
							list.SetContinue("next-page")
						} else {
							require.Equal(t, "next-page", options.Continue)
							require.NoError(t, meta.SetList(list, items[:1]))
							list.SetContinue("")
						}
					}
					return nil
				},
			}, objects...)
			r := aggregateRouter(t, middleware.PolicyTarget{Store: store, ExecutionClient: execution}, "alice", []string{"team-a", "team-b"}, zap.New(core))
			w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/"+resource, "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var response secrets.SecretListResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.Equal(t, 3, response.Total)
			names := make([]string, 0, len(response.Items))
			for _, item := range response.Items {
				names = append(names, item.Namespace+"/"+item.Name)
			}
			require.Equal(t, []string{"team-a/a", "team-a/z", "team-b/b"}, names)
			require.Equal(t, []string{"team-a:", "team-a:next-page", "team-b:"}, calls)
			require.NotContains(t, w.Body.String(), "excluded-payload")
			require.NotContains(t, w.Body.String(), "unassigned-payload")
			require.NotContains(t, w.Body.String(), "unmanaged-payload")
			require.NotContains(t, w.Body.String(), "next-page")
			audits := logs.FilterMessage("authorization decision").All()
			require.Len(t, audits, 3, "one audit per namespace plus the request audit")
			for i, namespace := range []string{"team-a", "team-b"} {
				fields := audits[i].ContextMap()
				state := store.Lookup(namespace)
				require.Equal(t, namespace, fields["namespace"])
				require.Equal(t, "allowed", fields["outcome"])
				require.Equal(t, string(state.PolicyUID), fields["policyUID"])
				require.Equal(t, state.PolicyResourceVersion, fields["policyVersion"])
			}
			require.Empty(t, audits[2].ContextMap()["namespace"])

			logs.TakeAll()
			w = policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/"+resource, "")
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, 3, logs.FilterMessage("authorization decision").Len(), "audits must reset per request")
		})
	}

	t.Run("empty and unavailable collections perform no reads", func(t *testing.T) {
		store, _, _ := policyStore(t, nil)
		pending, _, _ := policyStore(t, func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Annotations = nil })
		unsynced, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
		require.NoError(t, err)
		execution := aggregateClient(t, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				t.Fatal("empty or unavailable collection reached resource storage")
				return nil
			},
		})
		for _, tc := range []struct {
			name, caller string
			store        *authorization.Store
			allowed      []string
			status       int
		}{
			{name: "no matching role including operator", caller: "operator", store: store, status: http.StatusOK},
			{name: "grant outside boundary", caller: "alice", store: store, allowed: []string{"team-b"}, status: http.StatusOK},
			{name: "pending association", caller: "alice", store: pending, status: http.StatusOK},
			{name: "initial observation incomplete", caller: "alice", store: unsynced, status: http.StatusServiceUnavailable},
			{name: "store absent", caller: "alice", status: http.StatusServiceUnavailable},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := aggregateRouter(t, middleware.PolicyTarget{Store: tc.store, ExecutionClient: execution, OperatorUsers: []string{"operator"}}, tc.caller, tc.allowed, zap.NewNop())
				w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/secrets", "")
				require.Equal(t, tc.status, w.Code, w.Body.String())
				if tc.status == http.StatusOK {
					require.JSONEq(t, `{"items":[],"total":0}`, w.Body.String())
				} else {
					require.Contains(t, w.Body.String(), "POLICY_UNAVAILABLE")
				}
			})
		}
	})

	t.Run("execution failure withholds the whole collection", func(t *testing.T) {
		for _, failure := range []struct {
			name            string
			err             error
			status          int
			secondNamespace bool
		}{
			{"forbidden", k8serrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("denied")), http.StatusForbidden, false},
			{"unauthorized", k8serrors.NewUnauthorized("expired"), http.StatusUnauthorized, false},
			{"upstream page failure", errors.New("private-upstream-error"), http.StatusInternalServerError, false},
			{"upstream namespace failure", errors.New("private-upstream-error"), http.StatusInternalServerError, true},
		} {
			t.Run(failure.name, func(t *testing.T) {
				core, logs := observer.New(zap.InfoLevel)
				store, observations, policy := policyStore(t, nil)
				if failure.secondNamespace {
					addAggregateNamespace(t, store, observations, policy, "team-b")
				}
				calls := 0
				execution := aggregateClient(t, interceptor.Funcs{
					List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						options := (&client.ListOptions{}).ApplyOptions(opts)
						audits := logs.FilterField(zap.String("namespace", options.Namespace)).All()
						require.Len(t, audits, 1, "authorization must be audited before a potentially failing read")
						require.Equal(t, "allowed", audits[0].ContextMap()["outcome"])
						calls++
						if calls == 2 {
							return failure.err
						}
						require.NoError(t, meta.SetList(list, []runtime.Object{aggregateSecret("team-a")}))
						if !failure.secondNamespace {
							list.SetContinue("next")
						}
						return nil
					},
				})
				r := aggregateRouter(t, middleware.PolicyTarget{Store: store, ExecutionClient: execution}, "alice", nil, zap.New(core))
				w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/secrets", "")
				require.Equal(t, failure.status, w.Code, w.Body.String())
				require.Equal(t, 2, calls, "failed page must not be retried")
				require.NotContains(t, w.Body.String(), "secret-payload")
				require.NotContains(t, w.Body.String(), "private-upstream-error")
			})
		}
	})

	t.Run("observed changes discard only affected batches", func(t *testing.T) {
		for _, timing := range []string{"before next page", "before publication", "before namespace read"} {
			for _, change := range []string{"revoke", "replace policy", "replace namespace", "remove managed label", "version only", "viewer to editor"} {
				t.Run(timing+"/"+change, func(t *testing.T) {
					core, logs := observer.New(zap.InfoLevel)
					store, observations, policy := policyStore(t, nil)
					addAggregateNamespace(t, store, observations, policy, "team-b")
					changedNamespace := "team-a"
					if timing == "before namespace read" {
						changedNamespace = "team-b"
					}
					var calls []string
					execution := aggregateClient(t, interceptor.Funcs{
						List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
							options := (&client.ListOptions{}).ApplyOptions(opts)
							require.NotEmpty(t, options.Namespace)
							calls = append(calls, options.Namespace)
							item := aggregateSecret(options.Namespace)
							if options.Continue != "" {
								item.Name += "-next"
							}
							require.NoError(t, meta.SetList(list, []runtime.Object{item}))
							if timing == "before next page" && options.Namespace == "team-a" && options.Continue == "" {
								list.SetContinue("next")
							}
							if (timing == "before publication" && options.Namespace == "team-b") ||
								(timing != "before publication" && len(calls) == 1) {
								changeAggregateAccess(t, store, observations, changedNamespace, change)
							}
							return nil
						},
					})
					r := aggregateRouter(t, middleware.PolicyTarget{Store: store, ExecutionClient: execution}, "alice", nil, zap.New(core))
					w := policyRequest(r, http.MethodGet, "/api/v1/clusters/c1/secrets", "")
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					var response secrets.SecretListResponse
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
					wantOutcome := "denied"
					if change == "version only" || change == "viewer to editor" {
						wantOutcome = "allowed"
						want := 2
						if timing == "before next page" {
							want = 3
						}
						require.Equal(t, want, response.Total)
						require.Len(t, calls, want)
						require.Contains(t, w.Body.String(), changedNamespace+"-secret-payload")
					} else {
						require.Equal(t, 1, response.Total)
						require.NotContains(t, w.Body.String(), changedNamespace+"-secret-payload")
						if timing == "before namespace read" {
							require.Equal(t, []string{"team-a"}, calls)
						} else {
							require.Equal(t, []string{"team-a", "team-b"}, calls)
						}
					}
					audits := logs.FilterField(zap.String("namespace", changedNamespace)).All()
					wantAudits := 2
					if timing == "before namespace read" {
						wantAudits = 1
					}
					require.Len(t, audits, wantAudits)
					fields := audits[len(audits)-1].ContextMap()
					state := store.Lookup(changedNamespace)
					require.Equal(t, wantOutcome, fields["outcome"])
					require.Equal(t, string(state.PolicyUID), fields["policyUID"])
					require.Equal(t, state.PolicyResourceVersion, fields["policyVersion"])
				})
			}
		}
	})
}

func aggregateRouter(t *testing.T, target middleware.PolicyTarget, caller string, allowed []string, logger *zap.Logger) *gin.Engine {
	t.Helper()
	cc := &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: "c1", AllowedNamespaces: allowed}}
	cc.SetHealthy(true)
	am := &testutil.MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
		return auth.ClusterCredential{ImpersonateUser: caller}, nil
	}}
	r := policyRouter(t, am, policyManager(t, cc), target, logger)
	group := r.Group("/api/v1/clusters/:cluster")
	capps.New(false, nil, nil, config.CappSizes{}).RegisterRoutes(group)
	configmaps.New().RegisterRoutes(group)
	secrets.New().RegisterRoutes(group)
	return r
}

func addAggregateNamespace(t *testing.T, store *authorization.Store, observations *fake.Clientset, policy *corev1.ConfigMap, name string) {
	t.Helper()
	ns := store.Lookup("team-a").Namespace.DeepCopy()
	ns.Name = name
	_, err := observations.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{})
	require.NoError(t, err)
	cm := policy.DeepCopy()
	cm.Name = "capp-assignment-" + name
	cm.Data["policy.json"] = aggregatePolicyJSON(t, name, []authorization.Binding{
		{Kind: rbacv1.UserKind, Name: "alice", Role: authorization.RoleViewer},
	})
	_, err = observations.CoreV1().ConfigMaps(cm.Namespace).Create(t.Context(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Lookup(name).Ready() }, 5*time.Second, time.Millisecond)
}

func aggregateSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: namespace}, Data: map[string][]byte{"value": []byte(namespace + "-secret-payload")}}
}

func changeAggregateAccess(t *testing.T, store *authorization.Store, observations *fake.Clientset, namespace, change string) {
	t.Helper()
	cm, err := observations.CoreV1().ConfigMaps("capp-system").Get(t.Context(), "capp-assignment-"+namespace, metav1.GetOptions{})
	require.NoError(t, err)
	cm.ResourceVersion = "2"
	switch change {
	case "revoke":
		cm.Data["policy.json"] = aggregatePolicyJSON(t, namespace, []authorization.Binding{})
	case "replace policy":
		cm.UID = "replacement-policy"
	case "replace namespace", "remove managed label":
		ns, getErr := observations.CoreV1().Namespaces().Get(t.Context(), namespace, metav1.GetOptions{})
		require.NoError(t, getErr)
		if change == "replace namespace" {
			ns.UID = "replacement-namespace"
			cm.Annotations[authorization.NamespaceUIDAnnotation] = string(ns.UID)
		} else {
			ns.Labels = nil
		}
		_, err = observations.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			state := store.Lookup(namespace)
			return state.Namespace.UID == ns.UID && len(state.Namespace.Labels) == len(ns.Labels)
		}, 5*time.Second, time.Millisecond)
	case "viewer to editor":
		cm.Data["policy.json"] = aggregatePolicyJSON(t, namespace, []authorization.Binding{
			{Kind: rbacv1.UserKind, Name: "alice", Role: authorization.RoleEditor},
		})
	}
	_, err = observations.CoreV1().ConfigMaps(cm.Namespace).Update(t.Context(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Lookup(namespace).PolicyResourceVersion == cm.ResourceVersion }, 5*time.Second, time.Millisecond)
}

func aggregatePolicyJSON(t *testing.T, namespace string, bindings []authorization.Binding) string {
	t.Helper()
	payload, err := json.Marshal(authorization.Policy{
		Version:   authorization.PolicySchemaVersion,
		Namespace: namespace,
		Bindings:  bindings,
	})
	require.NoError(t, err)
	return string(payload)
}

func aggregateClient(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.Client {
	t.Helper()
	return clientfake.NewClientBuilder().WithScheme(testutil.TestScheme(t)).WithRESTMapper(policyExecutionMapper()).WithObjects(objects...).WithInterceptorFuncs(funcs).Build()
}

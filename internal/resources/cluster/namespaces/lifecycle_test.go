package namespaces

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestHandlerCreateManagedNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bindings   []authorization.Binding
		quota      bool
	}{
		{name: "omitted membership", body: `{"name":"team-a"}`, bindings: []authorization.Binding{}},
		{name: "empty membership", body: `{"name":"team-a","users":[]}`, bindings: []authorization.Binding{}},
		{name: "users and quota", body: `{"name":"team-a","users":["alice"],"quota":{"cpu":"2"}}`, bindings: []authorization.Binding{{Kind: "User", Name: "alice", Role: "editor"}}, quota: true},
		{name: "group viewer", body: `{"name":"team-a","bindings":[{"kind":"Group","name":"readers","role":"viewer"}]}`, bindings: []authorization.Binding{{Kind: "Group", Name: "readers", Role: "viewer"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				switch obj.(type) {
				case *corev1.Namespace:
					writes = append(writes, "namespace")
				case *corev1.ResourceQuota:
					writes = append(writes, "quota")
				case *corev1.ConfigMap:
					writes = append(writes, "policy")
				default:
					t.Fatalf("unexpected create: %T", obj)
				}
				return c.Create(ctx, obj, opts...)
			}})
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPost, "", tc.body)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			want := []string{"namespace", "policy"}
			if tc.quota {
				want = []string{"namespace", "quota", "policy"}
			}
			require.Equal(t, want, writes)
			ns, cm := membershipObjects(t, []authorization.Binding{})
			require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(ns), ns))
			require.Equal(t, consts.ManagedLabelValue, ns.Labels[consts.ManagedNameSpaceLabelKey])
			require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(cm), cm))
			assignment, err := authorization.ParseAssignment(cm, "capp-system", nil, nil)
			require.NoError(t, err)
			require.Equal(t, tc.bindings, assignment.Bindings())
			require.Empty(t, cm.Annotations[authorization.NamespaceUIDAnnotation], "association remains the worker's responsibility")
		})
	}
}

func TestHandlerCreateManagedNamespaceValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"restricted user", `{"name":"team-a","users":["system:admin"],"quota":{"cpu":"2"}}`, http.StatusBadRequest},
		{"restricted group", `{"name":"team-a","bindings":[{"kind":"Group","name":"system:authenticated","role":"editor"}]}`, http.StatusBadRequest},
		{"padded user", `{"name":"team-a","users":["alice "],"quota":{"cpu":"2"}}`, http.StatusBadRequest},
		{"control in group", `{"name":"team-a","bindings":[{"kind":"Group","name":"dev\nops","role":"editor"}]}`, http.StatusBadRequest},
		{"missing name", `{}`, http.StatusBadRequest},
		{"invalid name", `{"name":"INVALID"}`, http.StatusBadRequest},
		{"invalid quota", `{"name":"team-a","quota":{"memory":"invalid"}}`, http.StatusBadRequest},
		{"negative quota", `{"name":"team-a","quota":{"pods":-1}}`, http.StatusBadRequest},
		{"both representations", `{"name":"team-a","users":[],"bindings":[]}`, http.StatusBadRequest},
		{"invalid role", `{"name":"team-a","bindings":[{"kind":"User","name":"alice","role":"admin"}]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					calls++
					return errors.New("unexpected read")
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					calls++
					return errors.New("unexpected write")
				},
			})
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPost, "", tc.body)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Zero(t, calls)
		})
	}
}

func TestHandlerCreateManagedNamespaceConflicts(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing namespace", true: "orphan policy"}[orphan], func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			var existing client.Object = ns
			if orphan {
				existing = cm
			}
			writes := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				writes++
				return nil
			}}, existing)
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPost, "", `{"name":"team-a"}`)
			require.Equal(t, http.StatusConflict, w.Code)
			require.Zero(t, writes)
		})
	}
}

func TestHandlerCreateManagedNamespaceRecovery(t *testing.T) {
	for _, failedStep := range []string{"quota", "policy"} {
		t.Run(failedStep, func(t *testing.T) {
			fail := true
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				// Fake storage does not allocate namespace UIDs.
				if ns, ok := obj.(*corev1.Namespace); ok {
					ns.UID = "created-uid"
				}
				_, quota := obj.(*corev1.ResourceQuota)
				_, policy := obj.(*corev1.ConfigMap)
				if fail && ((quota && failedStep == "quota") || (policy && failedStep == "policy")) {
					return errors.New("private storage failure")
				}
				if quota {
					obj.SetUID("quota-uid")
				}
				return c.Create(ctx, obj, opts...)
			}})
			core, logs := observer.New(zap.InfoLevel)
			router := membershipRouter(t, storage, "operator", zap.New(core), nil)
			body := `{"name":"team-a","users":["private-member"],"quota":{"cpu":"2"}}`
			w := membershipRequest(router, http.MethodPost, "", body)
			require.Equal(t, http.StatusInternalServerError, w.Code)
			steps := []string{"namespaceCreated"}
			if failedStep == "policy" {
				steps = append(steps, "quotaSaved")
			}
			requireCompletedSteps(t, w.Body.Bytes(), steps)
			require.NotContains(t, w.Body.String(), "private")
			audit := logs.FilterMessage("namespace lifecycle mutation").All()
			require.Len(t, audit, 1)
			require.Equal(t, "partial", audit[0].ContextMap()["outcome"])
			require.Equal(t, "operator", audit[0].ContextMap()["caller"])
			require.Equal(t, "team-a", audit[0].ContextMap()["namespace"])
			for _, entry := range logs.All() {
				data, err := json.Marshal(entry.ContextMap())
				require.NoError(t, err)
				require.NotContains(t, string(data), "private")
			}
			fail = false
			w = membershipRequest(router, http.MethodPost, "", body)
			require.Equal(t, http.StatusConflict, w.Code, "POST never adopts retained resources")
			w = membershipRequest(router, http.MethodPut, "team-a", `{"users":["alice"],"quota":{"cpu":"2"}}`)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			_, policy := membershipObjects(t, []authorization.Binding{})
			require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(policy), policy))
			assignment, err := authorization.ParseAssignment(policy, "capp-system", nil, nil)
			require.NoError(t, err)
			require.True(t, assignment.Allows("alice", nil, []string{authorization.RoleEditor}))
		})
	}
}

func TestHandlerUpdateMembershipQuota(t *testing.T) {
	ns, cm := membershipObjects(t, []authorization.Binding{})
	quota := existingResourceQuota(ns.Name, "4", "4Gi")
	quota.UID = "quota-uid"
	quota.Annotations = map[string]string{"keep": "value"}
	quota.Status.Used = corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("3")}
	var writes []string
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		switch obj.(type) {
		case *corev1.ResourceQuota:
			writes = append(writes, "quota")
			require.Equal(t, quota.UID, obj.GetUID())
			require.Equal(t, quota.ResourceVersion, obj.GetResourceVersion())
			require.Equal(t, quota.Annotations, obj.GetAnnotations())
		case *corev1.ConfigMap:
			writes = append(writes, "policy")
		default:
			t.Fatalf("unexpected update: %T", obj)
		}
		return c.Update(ctx, obj, opts...)
	}}, ns, cm, quota)
	router := membershipRouter(t, storage, "operator", zap.NewNop(), nil)
	for _, body := range []string{
		`{"users":["alice"],"quota":{"cpu":"2","memory":"4Gi"}}`,
		`{"users":["alice"],"quota":{"cpu":"invalid"}}`,
		`{"bindings":[{"kind":"User","name":"alice","role":"admin"}],"quota":{"cpu":"5"}}`,
	} {
		w := membershipRequest(router, http.MethodPut, ns.Name, body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Empty(t, writes)
	}
	w := membershipRequest(router, http.MethodPut, ns.Name, `{"users":["alice"],"quota":{"cpu":"5","memory":"4Gi"}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, []string{"quota", "policy"}, writes)
}

func TestHandlerUpdateMembershipQuotaPartialFailure(t *testing.T) {
	ns, cm := membershipObjects(t, []authorization.Binding{})
	writes := 0
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		writes++
		return k8serrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("private conflict"))
	}}, ns, cm)
	w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPut, ns.Name, `{"users":["alice"],"quota":{"cpu":"2"}}`)
	require.Equal(t, http.StatusConflict, w.Code)
	requireCompletedSteps(t, w.Body.Bytes(), []string{"quotaSaved"})
	require.Equal(t, 1, writes, "conflicts are not retried")
	quota := &corev1.ResourceQuota{}
	require.NoError(t, storage.Get(t.Context(), client.ObjectKey{Name: ns.Name + "-quota", Namespace: ns.Name}, quota))
	require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(cm), cm))
	assignment, err := authorization.ParseAssignment(cm, "capp-system", nil, nil)
	require.NoError(t, err)
	require.Empty(t, assignment.Bindings())
}

func TestHandlerDeleteManagedNamespace(t *testing.T) {
	for _, associated := range []bool{false, true} {
		t.Run(map[bool]string{false: "unassociated", true: "associated"}[associated], func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			if !associated {
				delete(cm.Annotations, authorization.NamespaceUIDAnnotation)
			}
			var writes []string
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				options := (&client.DeleteOptions{}).ApplyOptions(opts)
				require.NotNil(t, options.Preconditions)
				require.NotNil(t, options.Preconditions.UID)
				switch obj.(type) {
				case *corev1.Namespace:
					writes = append(writes, "namespace")
					require.Equal(t, ns.UID, *options.Preconditions.UID)
				case *corev1.ConfigMap:
					writes = append(writes, "policy")
					require.Equal(t, cm.UID, *options.Preconditions.UID)
					require.NotNil(t, options.Preconditions.ResourceVersion)
					require.Equal(t, cm.ResourceVersion, *options.Preconditions.ResourceVersion)
				default:
					t.Fatalf("unexpected delete: %T", obj)
				}
				return nil
			}}, ns, cm)
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodDelete, ns.Name, "")
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			require.Equal(t, []string{"namespace", "policy"}, writes)
		})
	}
}

func TestHandlerDeleteManagedNamespaceGuards(t *testing.T) {
	for _, scenario := range []string{"nonempty", "unmanaged", "mismatched policy", "unowned policy", "absent namespace", "missing policy"} {
		t.Run(scenario, func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			objects := []client.Object{ns, cm}
			status := http.StatusConflict
			switch scenario {
			case "nonempty":
				objects = append(objects, &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns.Name}})
			case "unmanaged":
				ns.Labels = nil
			case "mismatched policy":
				cm.Annotations[authorization.NamespaceUIDAnnotation] = "different-uid"
			case "unowned policy":
				cm.Labels = nil
			case "absent namespace":
				objects = []client.Object{cm}
				status = http.StatusNotFound
			case "missing policy":
				objects = []client.Object{ns}
				status = http.StatusNoContent
			}
			writes := 0
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				writes++
				return nil
			}}, objects...)
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodDelete, ns.Name, "")
			require.Equal(t, status, w.Code, w.Body.String())
			wantWrites := 0
			if scenario == "missing policy" {
				wantWrites = 1
			}
			require.Equal(t, wantWrites, writes)
		})
	}
}

func TestHandlerDeleteManagedNamespaceRecovery(t *testing.T) {
	ns, cm := membershipObjects(t, []authorization.Binding{})
	ns.Finalizers = []string{"example.org/keep"}
	policyDeletes := 0
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*corev1.ConfigMap); ok {
			policyDeletes++
			if policyDeletes == 1 {
				return errors.New("private cleanup failure")
			}
		}
		return c.Delete(ctx, obj, opts...)
	}}, ns, cm)
	core, logs := observer.New(zap.InfoLevel)
	router := membershipRouter(t, storage, "operator", zap.New(core), nil)
	w := membershipRequest(router, http.MethodDelete, ns.Name, "")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	requireCompletedSteps(t, w.Body.Bytes(), []string{"namespaceDeletionAccepted"})
	require.Equal(t, 1, policyDeletes)
	require.NoError(t, storage.Get(t.Context(), client.ObjectKeyFromObject(ns), ns))
	require.NotNil(t, ns.DeletionTimestamp)
	w = membershipRequest(router, http.MethodDelete, ns.Name, "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.Equal(t, 2, policyDeletes)
	audit := logs.FilterMessage("namespace lifecycle mutation").All()
	require.Len(t, audit, 2)
	require.Equal(t, "partial", audit[0].ContextMap()["outcome"])
	require.Equal(t, string(cm.UID), audit[0].ContextMap()["policyUID"])
	require.Equal(t, "saved", audit[1].ContextMap()["outcome"])
}

func TestHandlerDeleteManagedNamespaceConflicts(t *testing.T) {
	for _, replaced := range []string{"namespace", "policy"} {
		t.Run(replaced, func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			var attempts []string
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				kind := "namespace"
				if _, ok := obj.(*corev1.ConfigMap); ok {
					kind = "policy"
				}
				attempts = append(attempts, kind)
				options := (&client.DeleteOptions{}).ApplyOptions(opts)
				require.NotNil(t, options.Preconditions)
				require.Equal(t, obj.GetUID(), *options.Preconditions.UID)
				if kind == replaced {
					return k8serrors.NewConflict(schema.GroupResource{Resource: kind}, obj.GetName(), errors.New("replacement"))
				}
				return nil
			}}, ns, cm)
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodDelete, ns.Name, "")
			require.Equal(t, http.StatusConflict, w.Code)
			want := []string{"namespace"}
			if replaced == "policy" {
				want = append(want, "policy")
				requireCompletedSteps(t, w.Body.Bytes(), []string{"namespaceDeletionAccepted"})
			}
			require.Equal(t, want, attempts, "never retry with replacement identities")
		})
	}
}

func TestHandlerNamespaceLifecycleDenials(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, scenario := range []string{"nonoperator", "missing decision", "protected"} {
			t.Run(method+"/"+scenario, func(t *testing.T) {
				calls := 0
				storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						calls++
						return errors.New("unexpected read")
					},
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						calls++
						return errors.New("unexpected create")
					},
					Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
						calls++
						return errors.New("unexpected delete")
					},
				})
				user, name := "operator", "team-a"
				var before gin.HandlerFunc
				switch scenario {
				case "nonoperator":
					user = "other"
				case "missing decision":
					before = func(c *gin.Context) { c.Request = c.Request.Clone(c.Request.Context()) }
				case "protected":
					name = "capp-system"
				}
				router := membershipRouter(t, storage, user, zap.NewNop(), before)
				w := membershipRequest(router, method, name, `{"name":"`+name+`"}`)
				require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				require.Zero(t, calls)
			})
		}
	}
}

func requireCompletedSteps(t *testing.T, body []byte, want []string) {
	t.Helper()
	var response struct {
		Error struct {
			Details struct {
				CompletedSteps []string `json:"completedSteps"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	require.Equal(t, want, response.Error.Details.CompletedSteps)
}

func TestHandlerUpdateMembershipQuotaFailure(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "create quota", true: "update quota"}[existing], func(t *testing.T) {
			ns, cm := membershipObjects(t, []authorization.Binding{})
			objects := []client.Object{ns, cm}
			if existing {
				quota := existingResourceQuota(ns.Name, "2", "8Gi")
				quota.UID = "quota-uid"
				objects = append(objects, quota)
			}
			attempts := 0
			reject := func(obj client.Object) error {
				attempts++
				require.IsType(t, &corev1.ResourceQuota{}, obj, "policy must not be written after a quota failure")
				return k8serrors.NewForbidden(schema.GroupResource{Resource: "resourcequotas"}, obj.GetName(), errors.New("private infrastructure denial"))
			}
			storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					return reject(obj)
				},
				Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
					return reject(obj)
				},
			}, objects...)
			w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPut, ns.Name, `{"users":["alice"],"quota":{"cpu":"4","memory":"4Gi"}}`)
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			require.NotContains(t, w.Body.String(), "private")
			require.Equal(t, 1, attempts)
			requireCompletedSteps(t, w.Body.Bytes(), nil)
		})
	}
}

func TestHandlerUpdateMembershipQuotaOnly(t *testing.T) {
	ns, _ := membershipObjects(t, []authorization.Binding{})
	storage := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		require.IsType(t, &corev1.ResourceQuota{}, obj, "omitted membership must not initialize policy")
		return c.Create(ctx, obj, opts...)
	}}, ns)
	w := membershipRequest(membershipRouter(t, storage, "operator", zap.NewNop(), nil), http.MethodPut, ns.Name, `{"quota":{"cpu":"2"}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	quota := &corev1.ResourceQuota{}
	require.NoError(t, storage.Get(t.Context(), client.ObjectKey{Name: ns.Name + "-quota", Namespace: ns.Name}, quota))
	cpu := quota.Spec.Hard[corev1.ResourceLimitsCPU]
	require.Equal(t, "2", cpu.String())
}

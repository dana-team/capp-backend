package namespaces

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/desiredstate/desiredstatetest"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/internal/resources/tracking/trackingtest"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	nsList = "/api/v1/clusters/c1/namespaces"
	nsItem = nsList + "/team-a"
)

type nsEngine struct {
	*testutil.EngineHelper
	t     *testing.T
	k8s   client.Client
	store *desiredstatetest.MemoryStore
}

func newNSEngine(t *testing.T, k8sClient client.Client) *nsEngine {
	t.Helper()
	tracker, store := trackingtest.New(t)
	return &nsEngine{trackingtest.Engine(t, k8sClient, New(zap.NewNop()).WithTracker(tracker)), t, k8sClient, store}
}

func (e *nsEngine) create() {
	e.t.Helper()
	w := e.Post(nsList, strings.NewReader(`{"name":"team-a","users":["alice"],"quota":{"cpu":"2"}}`))
	require.Equal(e.t, http.StatusCreated, w.Code, w.Body.String())
}

func (e *nsEngine) users() []string {
	e.t.Helper()
	rb := &rbacv1.RoleBinding{}
	require.NoError(e.t, e.k8s.Get(e.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "team-a-capp-access"}, rb))
	users := make([]string, 0, len(rb.Subjects))
	for _, subject := range rb.Subjects {
		users = append(users, subject.Name)
	}
	return users
}

// grant adds a user directly in the cluster.
func (e *nsEngine) grant(user string) {
	e.t.Helper()
	rb := &rbacv1.RoleBinding{}
	require.NoError(e.t, e.k8s.Get(e.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "team-a-capp-access"}, rb))
	rb.Subjects = append(rb.Subjects, rbacv1.Subject{Kind: rbacv1.UserKind, Name: user})
	require.NoError(e.t, e.k8s.Update(e.t.Context(), rb))
}

func (e *nsEngine) drift() *drift.Drift { return trackingtest.Drift(e.t, e.Get(nsItem)) }

func TestNamespaceDriftLifecycle(t *testing.T) {
	e := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	e.create()
	assert.Equal(t, drift.StateInSync, e.drift().State)

	e.grant("eve")
	quota := &corev1.ResourceQuota{}
	key := client.ObjectKey{Namespace: "team-a", Name: "team-a-quota"}
	require.NoError(t, e.k8s.Get(t.Context(), key, quota))
	quota.Spec.Hard[corev1.ResourceLimitsCPU] = resource.MustParse("8")
	require.NoError(t, e.k8s.Update(t.Context(), quota))

	d := e.drift()
	require.Equal(t, drift.StateChanged, d.State)
	require.Len(t, d.Fields, 2)
	assert.Equal(t, drift.Field{Path: "/quota/cpu", Desired: "2", Live: "8"}, d.Fields[0])
	assert.Equal(t, "/users", d.Fields[1].Path)

	w := e.PostJSON(nsItem+"/drift/revert", trackingtest.Action(d))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, w).State)
	assert.Equal(t, []string{"alice"}, e.users())

	e.grant("bob")
	w = e.PostJSON(nsItem+"/drift/accept", trackingtest.Action(e.drift()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, drift.StateInSync, e.drift().State)
	assert.Equal(t, []string{"alice", "bob"}, e.users())
}

func TestNamespaceRevertRemovesAnAddedQuota(t *testing.T) {
	e := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	require.Equal(t, http.StatusCreated, e.Post(nsList, strings.NewReader(`{"name":"team-a","users":["alice"]}`)).Code)
	require.NoError(t, e.k8s.Create(t.Context(), existingQuota("1")))
	d := e.drift()
	require.Equal(t, drift.StateChanged, d.State)
	require.Equal(t, http.StatusOK, e.PostJSON(nsItem+"/drift/revert", trackingtest.Action(d)).Code)
	err := e.k8s.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "team-a-quota"}, &corev1.ResourceQuota{})
	assert.True(t, k8serrors.IsNotFound(err))
}

func existingQuota(cpu string) *corev1.ResourceQuota {
	quota := existingResourceQuota("team-a", cpu, "")
	quota.ResourceVersion = ""
	return quota
}

func TestNamespaceUpdateAsksAboutDrift(t *testing.T) {
	for name, tc := range map[string]struct {
		method, body string
		accepted     []string
		reverted     []string
	}{
		"put":   {http.MethodPut, `{"quota":{"cpu":"4"}}`, []string{"alice", "eve"}, []string{"alice"}},
		"patch": {http.MethodPatch, `{"users":["bob"]}`, []string{"alice", "eve", "bob"}, []string{"alice", "bob"}},
	} {
		t.Run(name, func(t *testing.T) {
			run := func(query string) (*nsEngine, *httptest.ResponseRecorder) {
				e := newNSEngine(t, testutil.FakeClientAllowSAR(t))
				e.create()
				e.grant("eve")
				return e, testutil.ServeHTTP(e.Engine, tc.method, nsItem+query, strings.NewReader(tc.body))
			}
			e, w := run("")
			require.Equal(t, http.StatusConflict, w.Code)
			assert.Equal(t, []string{"alice", "eve"}, e.users())

			e, w = run("?onDrift=accept")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, tc.accepted, e.users())
			assert.Equal(t, drift.StateInSync, e.drift().State)

			e, w = run("?onDrift=revert")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, tc.reverted, e.users())
			assert.Equal(t, drift.StateInSync, e.drift().State)
		})
	}
}

func TestMissingNamespace(t *testing.T) {
	e := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	e.create()
	for _, object := range []client.Object{&corev1.ResourceQuota{}, &rbacv1.RoleBinding{}} {
		require.NoError(t, e.k8s.DeleteAllOf(t.Context(), object, client.InNamespace("team-a")))
	}
	ns := &corev1.Namespace{}
	ns.Name = "team-a"
	require.NoError(t, e.k8s.Delete(t.Context(), ns))

	var list NamespaceListResponse
	require.NoError(t, json.Unmarshal(e.Get(nsList).Body.Bytes(), &list))
	require.Len(t, list.Missing, 1)
	assert.Equal(t, "team-a", list.Missing[0].Name)

	w := e.Get(nsItem)
	require.Equal(t, http.StatusNotFound, w.Code)
	d := trackingtest.Drift(t, w)
	require.NotNil(t, d)
	w = e.PostJSON(nsItem+"/drift/revert", trackingtest.Action(d))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"alice"}, e.users())
	assert.Equal(t, drift.StateInSync, e.drift().State)
}

func TestMissingNamespaceIsHiddenWithoutAccess(t *testing.T) {
	allowed := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	allowed.create()

	denied := testutil.FakeClientDenySAR(t)
	tracker := tracking.New(allowed.store, mustSealer(t), 1e9)
	e := trackingtest.Engine(t, denied, New(zap.NewNop()).WithTracker(tracker))
	w := e.Get(nsItem)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Nil(t, trackingtest.Drift(t, w))
	assert.NotContains(t, e.Get(nsList).Body.String(), "missing")
	for _, action := range []string{"revert", "accept"} {
		require.Equal(t, http.StatusForbidden, e.PostJSON(nsItem+"/drift/"+action, drift.ActionRequest{}).Code)
	}
}

func TestNamespaceAcceptNeedsOnlyARead(t *testing.T) {
	owner := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	owner.create()
	owner.grant("eve")
	d := owner.drift()

	// A caller who can view the namespace but not administer namespaces.
	viewer := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Create: allowSAR(func(a *authorizationv1.ResourceAttributes) bool { return a.Namespace != "" }),
		Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			return owner.k8s.Get(ctx, key, obj)
		},
	})
	e := trackingtest.Engine(t, viewer, New(zap.NewNop()).WithTracker(tracking.New(owner.store, mustSealer(t), 1e9)))
	require.Equal(t, http.StatusForbidden, e.PostJSON(nsItem+"/drift/revert", trackingtest.Action(d)).Code)
	require.Equal(t, http.StatusOK, e.PostJSON(nsItem+"/drift/accept", trackingtest.Action(d)).Code)
}

func TestNamespacePatchCannotRevertWithoutAdministration(t *testing.T) {
	owner := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	owner.create()
	owner.grant("eve")

	// Capp creators may add users, but not reset the namespace.
	creator := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Create: allowSAR(func(a *authorizationv1.ResourceAttributes) bool { return a.Namespace != "" }),
		Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			return owner.k8s.Get(ctx, key, obj)
		},
		Update: func(ctx context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			return owner.k8s.Update(ctx, obj)
		},
	})
	e := trackingtest.Engine(t, creator, New(zap.NewNop()).WithTracker(tracking.New(owner.store, mustSealer(t), 1e9)))
	patch := func(query string) int {
		return testutil.ServeHTTP(e.Engine, http.MethodPatch, nsItem+query, strings.NewReader(`{"users":["bob"]}`)).Code
	}
	require.Equal(t, http.StatusForbidden, patch("?onDrift=revert"))
	assert.Equal(t, []string{"alice", "eve"}, owner.users())
	require.Equal(t, http.StatusOK, patch("?onDrift=accept"))
	assert.Equal(t, []string{"alice", "eve", "bob"}, owner.users())
}

func TestNamespaceDeleteForgetsEverything(t *testing.T) {
	e := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	e.create()
	ref := desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindSecret, Namespace: "team-a", Name: "db"}
	require.NoError(t, e.store.Save(t.Context(), &desiredstate.Record{ID: ref.ID(), Cluster: "c1", Kind: ref.Kind, Namespace: "team-a", Name: "db", Generation: 1}, 0))

	require.Equal(t, http.StatusNoContent, e.Delete(nsItem).Code)
	assert.Zero(t, e.store.Len())
}

func TestNamespaceStoreOutage(t *testing.T) {
	e := newNSEngine(t, testutil.FakeClientAllowSAR(t))
	e.create()
	e.store.Err = assert.AnError
	patch := func(query string) *httptest.ResponseRecorder {
		return testutil.ServeHTTP(e.Engine, http.MethodPatch, nsItem+query, strings.NewReader(`{"users":["bob"]}`))
	}

	w := e.Get(nsItem)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, drift.StateUnavailable, trackingtest.Drift(t, w).State)

	require.Equal(t, http.StatusServiceUnavailable, patch("?onDrift=revert").Code)
	assert.NotContains(t, e.users(), "bob")
	for name, w := range map[string]*httptest.ResponseRecorder{
		"patch":  patch(""),
		"accept": patch("?onDrift=accept"),
		"update": e.PutJSON(nsItem, UpdateNamespaceRequest{}),
		"delete": e.Delete(nsItem),
	} {
		assert.Equal(t, http.StatusOK, w.Code, name)
		assert.Contains(t, w.Body.String(), apierrors.CodeDesiredStateFailed, name)
	}
	require.Equal(t, http.StatusServiceUnavailable, e.PostJSON(nsItem+"/drift/accept", drift.ActionRequest{}).Code)
	require.Equal(t, http.StatusServiceUnavailable, e.PostJSON(nsItem+"/drift/revert", drift.ActionRequest{}).Code)
}

func TestFailedNamespaceReadIsNotRecorded(t *testing.T) {
	fail := false
	e := newNSEngine(t, testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Create: allowSAR(func(*authorizationv1.ResourceAttributes) bool { return true }),
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*rbacv1.RoleBinding); ok && fail {
				return k8serrors.NewForbidden(schema.GroupResource{Resource: "rolebindings"}, key.Name, nil)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}))
	e.create()
	fail = true
	w := e.PutJSON(nsItem, UpdateNamespaceRequest{Quota: &resourceQuota{CPU: "4"}})
	require.NotEqual(t, http.StatusOK, w.Code, "the current users cannot be compared")
	fail = false
	assert.Equal(t, drift.StateInSync, e.drift().State)
}

func TestPolicyNamespaceDrift(t *testing.T) {
	storage := testutil.FakeClient(t)
	router := func(user string) (http.Handler, *desiredstatetest.MemoryStore) {
		tracker, store := trackingtest.New(t)
		observations, err := authorization.NewStore(fake.NewClientset(), authorization.StoreOptions{PolicyNamespace: "capp-system"})
		require.NoError(t, err)
		return membershipRouterWithTracker(t, storage, user, zap.NewNop(), nil, observations, tracker), store
	}
	operator, store := router("operator")
	send := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		operator.ServeHTTP(w, req)
		return w
	}
	action := func(d *drift.Drift) string {
		body, err := json.Marshal(trackingtest.Action(d))
		require.NoError(t, err)
		return string(body)
	}

	w := send(http.MethodPost, nsList, `{"name":"team-a","bindings":[{"kind":"Group","name":"devs","role":"editor"}],"quota":{"cpu":"2"}}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, send(http.MethodGet, nsItem, "")).State)
	assert.Equal(t, 1, store.Len())

	// Somebody edits the policy ConfigMap directly.
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: "capp-system", Name: "capp-assignment-team-a"}
	require.NoError(t, storage.Get(t.Context(), key, cm))
	original := cm.Data["policy.json"]
	cm.Data["policy.json"] = strings.Replace(original, "devs", "everyone", 1)
	require.NoError(t, storage.Update(t.Context(), cm))

	d := trackingtest.Drift(t, send(http.MethodGet, nsItem, ""))
	require.Equal(t, drift.StateChanged, d.State)
	assert.Equal(t, []drift.Field{{Path: "/bindings/0/name", Desired: "devs", Live: "everyone"}}, d.Fields)
	require.Equal(t, http.StatusConflict, send(http.MethodPut, nsItem, `{"quota":{"cpu":"4"}}`).Code)

	w = send(http.MethodPost, nsItem+"/drift/revert", action(d))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, storage.Get(t.Context(), key, cm))
	assert.JSONEq(t, original, cm.Data["policy.json"])

	// An invalid policy can still be repaired with a full replacement.
	cm.Data["policy.json"] = "{}"
	require.NoError(t, storage.Update(t.Context(), cm))
	assert.Equal(t, drift.StateUnavailable, trackingtest.Drift(t, send(http.MethodGet, nsItem, "")).State)
	w = send(http.MethodPut, nsItem, `{"bindings":[{"kind":"User","name":"alice","role":"viewer"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, drift.StateInSync, trackingtest.Drift(t, send(http.MethodGet, nsItem, "")).State)

	// Deleting outside the API makes it missing; revert needs the policy gone too.
	ns := &corev1.Namespace{}
	require.NoError(t, storage.Get(t.Context(), client.ObjectKey{Name: "team-a"}, ns))
	require.NoError(t, storage.Delete(t.Context(), ns))
	require.NoError(t, storage.Delete(t.Context(), cm))
	require.NoError(t, storage.DeleteAllOf(t.Context(), &corev1.ResourceQuota{}, client.InNamespace("team-a")))
	d = trackingtest.Drift(t, send(http.MethodGet, nsItem, ""))
	require.NotNil(t, d)
	assert.Equal(t, drift.StateMissing, d.State)
	w = send(http.MethodPost, nsItem+"/drift/revert", action(d))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, storage.Get(t.Context(), key, cm))
	assignment, err := authorization.ParseAssignment(cm, "capp-system", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []authorization.Binding{{Kind: "User", Name: "alice", Role: "viewer"}}, assignment.Bindings())

	operator, _ = router("mallory")
	require.Equal(t, http.StatusForbidden, send(http.MethodPost, nsItem+"/drift/revert", `{}`).Code)
	// Without synchronized observations nobody but an operator is let through.
	require.Equal(t, http.StatusServiceUnavailable, send(http.MethodPost, nsItem+"/drift/accept", `{}`).Code)
}

func mustSealer(t *testing.T) *desiredstate.Sealer {
	t.Helper()
	sealer, err := desiredstate.NewSealer("k1", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)})
	require.NoError(t, err)
	return sealer
}

// allowSAR answers access reviews with decide and creates everything else.
func allowSAR(decide func(*authorizationv1.ResourceAttributes) bool) func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
	return func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if review, ok := obj.(*authorizationv1.SelfSubjectAccessReview); ok {
			review.Status.Allowed = decide(review.Spec.ResourceAttributes)
			return nil
		}
		return c.Create(ctx, obj, opts...)
	}
}

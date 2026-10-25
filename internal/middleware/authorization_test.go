package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	authorizationClusterRoute   = "/api/v1/clusters/:cluster"
	authorizationNamespaceRoute = authorizationClusterRoute + "/namespaces/:namespace"
	authorizationCappRoute      = authorizationNamespaceRoute + "/capps/:name"
)

func TestAuthorization(t *testing.T) {
	t.Run("requires every permission for each registered operation", testAuthorizationPermissions)
	t.Run("matches method and registered path rather than URL text", testAuthorizationRouteMatching)
	t.Run("public exemptions require exact method and path", testAuthorizationPublicRoutes)
	t.Run("namespace restrictions override grants before reads", testAuthorizationNamespaceRestrictions)
	t.Run("passes snapshots and current metadata to body handlers", testAuthorizationBodyContext)
	t.Run("reuses the source service client and rejects invalid context", testAuthorizationServiceClient)
	t.Run("aggregate handlers receive policy without a blanket grant", testAuthorizationAggregateContext)
	t.Run("policy and metadata failures never reach handlers", testAuthorizationFailures)
	t.Run("cluster discovery loads each target and rejects partial policy", testAuthorizationDiscovery)
	t.Run("subsequent requests reload policy and preserve verified identity", testAuthorizationRequestIsolation)
	t.Run("old namespace memberships cannot authorize a replacement", testAuthorizationNamespaceIdentity)
	t.Run("unknown clusters and missing namespaces retain not found responses", testAuthorizationBusinessErrors)
	t.Run("legacy modes bypass policy", testAuthorizationLegacyModes)
	t.Run("fails closed on missing identity or unsupported mode", testAuthorizationInvalidContext)
}

func testAuthorizationPermissions(t *testing.T) {
	t.Helper()
	type routeCase struct {
		method      string
		route       string
		permissions []string
	}
	cases := []routeCase{
		{http.MethodPost, authorizationClusterRoute + "/namespaces", []string{"namespaces:create"}},
		{http.MethodGet, authorizationNamespaceRoute, []string{"namespaces:get"}},
		{http.MethodPut, authorizationNamespaceRoute, []string{"namespaces:update"}},
		{http.MethodPatch, authorizationNamespaceRoute, []string{"roles/capp-user:grant"}},
		{http.MethodDelete, authorizationNamespaceRoute, []string{"namespaces:delete"}},
		{http.MethodGet, authorizationCappRoute + "/revisions", []string{"capprevisions:list"}},
		{http.MethodGet, authorizationCappRoute + "/revisions/:revision", []string{"capprevisions:get"}},
		{http.MethodPost, authorizationCappRoute + "/revisions/:revision/rollback", []string{"capprevisions:get", "capps:get", "capps:update"}},
		{http.MethodPost, authorizationCappRoute + "/sync", []string{"capps:get", "capps:sync"}},
		{http.MethodDelete, authorizationCappRoute + "/sync", []string{"capps:get", "capps:unsync"}},
		{http.MethodGet, authorizationCappRoute + "/benchmark", []string{"benchmarks:get", "capps:get"}},
		{http.MethodPost, authorizationCappRoute + "/benchmark", []string{"benchmarks:create", "capps:get"}},
		{http.MethodPost, authorizationCappRoute + "/migrate", []string{"capps:migrate", "capps:get", "secrets:list", "configmaps:list"}},
	}
	for _, resource := range []string{"capps", "secrets", "configmaps"} {
		for _, operation := range []struct{ method, suffix, action string }{
			{http.MethodGet, "", "list"}, {http.MethodPost, "", "create"},
			{http.MethodGet, "/:name", "get"}, {http.MethodPut, "/:name", "update"},
			{http.MethodDelete, "/:name", "delete"},
		} {
			cases = append(cases, routeCase{operation.method, authorizationNamespaceRoute + "/" + resource + operation.suffix, []string{resource + ":" + operation.action}})
		}
		if resource != "capps" {
			cases = append(cases, routeCase{http.MethodGet, authorizationNamespaceRoute + "/" + resource + "/names", []string{resource + ":list"}})
		}
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.route, func(t *testing.T) {
			scope := "allNamespaces"
			if tc.route == authorizationClusterRoute+"/namespaces" {
				scope = "cluster"
			}
			for missing := -1; missing < len(tc.permissions); missing++ {
				grants := slices.Clone(tc.permissions)
				wantStatus := http.StatusNoContent
				if missing >= 0 {
					grants = slices.Delete(grants, missing, missing+1)
					wantStatus = http.StatusForbidden
				}
				f := newAuthorizationFixture(t, grants, scope)
				w := f.serve(t, tc.method, tc.route, authorizationPath(tc.route), nil)
				if w.Code != wantStatus {
					t.Fatalf("grants %v: status = %d, want %d; %s", grants, w.Code, wantStatus, w.Body.String())
				}
				if f.called != (missing == -1) {
					t.Fatalf("grants %v: handler called = %t, want %t", grants, f.called, missing == -1)
				}
				if missing >= 0 {
					last := f.state.Decisions[len(f.state.Decisions)-1]
					if got := last.Resource + ":" + last.Action; got != tc.permissions[missing] || last.Reason != "POLICY_DENIED" {
						t.Errorf("denied decision = %+v, want missing permission %s", last, tc.permissions[missing])
					}
				}
			}
		})
	}
}

func testAuthorizationRouteMatching(t *testing.T) {
	t.Helper()
	for _, tc := range []struct{ method, route, path string }{
		{http.MethodPatch, authorizationCappRoute, authorizationPath(authorizationCappRoute)},
		{http.MethodGet, authorizationNamespaceRoute + "/widgets", authorizationPath(authorizationNamespaceRoute + "/widgets")},
		{http.MethodGet, "/api/v1/clusters/:cluster/*rest", authorizationPath(authorizationCappRoute)},
		{http.MethodGet, "", "/unregistered"},
	} {
		f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
		w := f.serve(t, tc.method, tc.route, tc.path, nil)
		assertAuthorizationError(t, w, http.StatusForbidden, apierrors.CodeForbidden, "POLICY_DENIED", "operation is not permitted")
		if f.called || f.lists != 0 || f.gets != 0 {
			t.Fatalf("unknown route %s %s: handler=%t lists=%d gets=%d, want no execution or reads", tc.method, tc.route, f.called, f.lists, f.gets)
		}
	}
}

func testAuthorizationPublicRoutes(t *testing.T) {
	t.Helper()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/healthz"}, {http.MethodGet, "/readyz"},
		{http.MethodGet, "/docs"}, {http.MethodGet, "/ui/scalar.js"},
		{http.MethodGet, "/openapi.yaml"}, {http.MethodGet, "/api/v1/sizes"},
		{http.MethodGet, "/custom-metrics"}, {http.MethodGet, "/api/v1/auth/mode"},
		{http.MethodGet, "/api/v1/auth/openshift/authorize"},
		{http.MethodPost, "/api/v1/auth/login"}, {http.MethodPost, "/api/v1/auth/refresh"},
		{http.MethodPost, "/api/v1/auth/openshift/callback"},
	} {
		f := newAuthorizationFixture(t, nil, "allNamespaces")
		f.credential = nil
		w := f.serve(t, tc.method, tc.path, tc.path, nil)
		if w.Code != http.StatusNoContent || f.state != nil || f.lists != 0 {
			t.Fatalf("public %s %s: status=%d state=%+v lists=%d", tc.method, tc.path, w.Code, f.state, f.lists)
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/docs"}, {http.MethodGet, "/docs/private"},
		{http.MethodGet, "/api/v1/auth/unknown"}, {http.MethodGet, "/metrics"},
	} {
		f := newAuthorizationFixture(t, nil, "allNamespaces")
		w := f.serve(t, tc.method, tc.path, tc.path, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("nonpublic %s %s: status = %d, want 403", tc.method, tc.path, w.Code)
		}
	}
}

func testAuthorizationNamespaceRestrictions(t *testing.T) {
	t.Helper()
	for _, namespace := range []string{"policies", "credentials", "outside"} {
		f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
		f.cfg.Clusters[0].Authorization.ProtectedNamespaces = []string{"credentials"}
		f.mgr.IsNamespaceAllowedFn = func(_ *cluster.ClusterClient, name string) bool { return name != "outside" }
		path := strings.Replace(authorizationPath(authorizationCappRoute), "/team/", "/"+namespace+"/", 1)
		w := f.serve(t, http.MethodGet, authorizationCappRoute, path, nil)
		if namespace == "outside" {
			if got := decodeError(t, w); got.Code != apierrors.CodeNamespaceDenied || w.Code != http.StatusForbidden {
				t.Fatalf("allowed-namespace denial = %+v, status %d", got, w.Code)
			}
		} else {
			assertAuthorizationError(t, w, http.StatusForbidden, apierrors.CodeNamespaceDenied, "PROTECTED_NAMESPACE", "namespace is not accessible")
			if len(f.state.Decisions) != 1 || f.state.Decisions[0].Reason != "PROTECTED_NAMESPACE" || f.state.Decisions[0].Outcome != "denied" {
				t.Fatalf("protected namespace decisions = %+v, want one protected denial", f.state.Decisions)
			}
		}
		if f.called || f.lists != 0 || f.gets != 0 {
			t.Fatalf("restricted %s: handler=%t lists=%d gets=%d, want no execution or reads", namespace, f.called, f.lists, f.gets)
		}
	}
}

func testAuthorizationBodyContext(t *testing.T) {
	t.Helper()
	f := newAuthorizationFixture(t, []string{"namespaces:update", "roles/capp-user:replace"}, "allNamespaces")
	w := f.serve(t, http.MethodPut, authorizationNamespaceRoute, authorizationPath(authorizationNamespaceRoute), func(c *gin.Context) {
		state := authorizationState(t, c)
		target := state.Targets["prod"]
		if target.Client == nil || target.Config.PolicyNamespace != "policies" || state.Namespace.UID != "current-uid" {
			t.Fatalf("unexpected handler context: %+v", state)
		}
		allowed, err := target.Snapshot.CheckNamespace(state.Namespace, "roles/capp-user", "replace")
		state.RecordDecision("prod", "team", "roles/capp-user", "replace", allowed, err)
		if err != nil || !allowed {
			t.Fatalf("reused snapshot delegation: allowed=%t error=%v", allowed, err)
		}
	})
	if w.Code != http.StatusNoContent || f.lists != 1 || f.gets != 1 {
		t.Fatalf("body route: status=%d lists=%d gets=%d, want 204/1/1", w.Code, f.lists, f.gets)
	}
	want := []middleware.AuthorizationDecision{
		{Cluster: "prod", Namespace: "team", Resource: "namespaces", Action: "update", PolicyVersion: "policy-version", Outcome: "allowed"},
		{Cluster: "prod", Namespace: "team", Resource: "roles/capp-user", Action: "replace", PolicyVersion: "policy-version", Outcome: "allowed"},
	}
	if diff := cmp.Diff(want, f.state.Decisions); diff != "" {
		t.Errorf("decision metadata (-want +got):\n%s", diff)
	}
	if f.state.Username != "alice" || f.state.Mode != config.AuthorizationModeApplication {
		t.Errorf("identity/mode = %q/%q", f.state.Username, f.state.Mode)
	}
}

func testAuthorizationServiceClient(t *testing.T) {
	t.Helper()
	for _, invalid := range []bool{false, true} {
		f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
		f.adminClient = f.client
		if invalid {
			f.adminClient = "invalid client"
		}
		f.mgr.ClientForFn = func(*cluster.ClusterClient, auth.ClusterCredential) (client.Client, error) {
			t.Fatal("ClientFor called despite an existing source service client")
			return nil, nil
		}
		w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
		if invalid {
			assertAuthorizationError(t, w, http.StatusServiceUnavailable, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
			if f.called || f.lists != 0 || f.gets != 0 {
				t.Fatalf("invalid client: handler=%t lists=%d gets=%d, want no execution or reads", f.called, f.lists, f.gets)
			}
			continue
		}
		if w.Code != http.StatusNoContent || f.lists != 1 || f.gets != 1 {
			t.Fatalf("source client reuse: status=%d lists=%d gets=%d, want 204/1/1", w.Code, f.lists, f.gets)
		}
	}
}

func testAuthorizationAggregateContext(t *testing.T) {
	t.Helper()
	for _, route := range []string{authorizationClusterRoute + "/namespaces", authorizationClusterRoute + "/capps", authorizationClusterRoute + "/secrets", authorizationClusterRoute + "/configmaps", authorizationClusterRoute, "/api/v1/clusters"} {
		f := newAuthorizationFixture(t, nil, "allNamespaces")
		w := f.serve(t, http.MethodGet, route, authorizationPath(route), func(c *gin.Context) {
			state := authorizationState(t, c)
			if state.Targets["prod"] == nil || state.Namespace != nil {
				t.Fatalf("aggregate context = %+v, want policy and no single namespace", state)
			}
		})
		if w.Code != http.StatusNoContent || f.lists != 1 || f.gets != 0 {
			t.Fatalf("aggregate %s: status=%d lists=%d gets=%d, want 204/1/0", route, w.Code, f.lists, f.gets)
		}
	}
}

func testAuthorizationFailures(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		name    string
		prepare func(*authorizationFixture)
	}{
		{name: "missing policy", prepare: func(f *authorizationFixture) {
			f.client = testutil.FakeClient(t)
		}},
		{name: "malformed policy", prepare: func(f *authorizationFixture) {
			policy := policyWithPermissions(t, nil, "allNamespaces")
			policy.Data["policy.json"] = "invalid sensitive policy"
			f.client = testutil.FakeClient(t, policy)
		}},
		{name: "unreadable policy", prepare: func(f *authorizationFixture) {
			f.client = testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("sensitive policy read diagnostic")
				},
			})
		}},
		{name: "namespace read failure", prepare: func(f *authorizationFixture) {
			f.client = testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("sensitive namespace read diagnostic")
				},
			}, policyWithPermissions(t, []string{"capps:get"}, "allNamespaces"))
		}},
		{name: "evaluation cannot resolve current UID", prepare: func(f *authorizationFixture) {
			f.client = testutil.FakeClient(t, policyWithPermissions(t, []string{"capps:get"}, "allNamespaces"),
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}})
		}},
		{name: "service client failure", prepare: func(f *authorizationFixture) {
			f.mgr.ClientForFn = func(*cluster.ClusterClient, auth.ClusterCredential) (client.Client, error) {
				return nil, errors.New("sensitive credential diagnostic")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
			tc.prepare(f)
			w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
			assertAuthorizationError(t, w, http.StatusServiceUnavailable, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
			if f.called || len(f.state.Decisions) != 1 || f.state.Decisions[0].Outcome != "unavailable" {
				t.Fatalf("failure: handler=%t decisions=%+v, want one unavailable decision", f.called, f.state.Decisions)
			}
		})
	}
}

func testAuthorizationDiscovery(t *testing.T) {
	t.Helper()
	f := newAuthorizationFixture(t, nil, "allNamespaces")
	f.cfg.Clusters = append(f.cfg.Clusters, config.ClusterConfig{Name: "other", Authorization: f.cfg.Clusters[0].Authorization})
	w := f.serve(t, http.MethodGet, "/api/v1/clusters", "/api/v1/clusters", nil)
	if w.Code != http.StatusNoContent || len(f.state.Targets) != 2 || f.lists != 2 {
		t.Fatalf("discovery: status=%d targets=%d lists=%d, want 204/2/2", w.Code, len(f.state.Targets), f.lists)
	}
	f.called = false
	f.mgr.ClientForFn = func(cc *cluster.ClusterClient, _ auth.ClusterCredential) (client.Client, error) {
		if cc.Meta.Name == "other" {
			return testutil.FakeClient(t), nil
		}
		return f.client, nil
	}
	w = f.serve(t, http.MethodGet, "/api/v1/clusters", "/api/v1/clusters", nil)
	assertAuthorizationError(t, w, 503, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
	if f.called || f.state.Decisions[0].Cluster != "other" {
		t.Fatalf("partial discovery: handler=%t decisions=%+v", f.called, f.state.Decisions)
	}
}

func testAuthorizationRequestIsolation(t *testing.T) {
	t.Helper()
	f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.CredentialKey), f.credential)
		c.Next()
		f.state = authorizationState(t, c)
	})
	engine.Use(middleware.Authorization(f.cfg, f.mgr))
	engine.GET(authorizationCappRoute, func(c *gin.Context) {
		f.called = true
		c.Status(http.StatusNoContent)
	})
	w := testutil.ServeHTTP(engine, http.MethodGet, authorizationPath(authorizationCappRoute), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("initial request = %d, want 204", w.Code)
	}
	previous := f.state.Targets["prod"].Snapshot
	f.credential = auth.ClusterCredential{ImpersonateUser: "bob"}
	f.called = false
	w = testutil.ServeHTTP(engine, http.MethodGet, authorizationPath(authorizationCappRoute), nil)
	assertAuthorizationError(t, w, 403, apierrors.CodeForbidden, "POLICY_DENIED", "operation is not permitted")
	if f.called || f.state.Username != "bob" || previous == f.state.Targets["prod"].Snapshot || f.lists != 2 {
		t.Fatalf("subsequent request: handler=%t user=%q lists=%d", f.called, f.state.Username, f.lists)
	}
}

func testAuthorizationNamespaceIdentity(t *testing.T) {
	t.Helper()
	f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
	policy := policyWithPermissions(t, []string{"capps:get"}, "allNamespaces")
	var data map[string]any
	if err := json.Unmarshal([]byte(policy.Data["policy.json"]), &data); err != nil {
		t.Fatal(err)
	}
	data["bindings"] = []any{}
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	policy.Data["policy.json"] = string(payload)
	membership := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "capp-members-old-uid", Namespace: "policies", Labels: policy.Labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "team", UID: "old-uid"}},
		},
		Data: map[string]string{"policy.json": `{"version":1,"namespace":{"name":"team","uid":"old-uid"},"users":[],"bindings":[{"kind":"User","name":"alice","role":"member"}]}`},
	}
	f.client = testutil.FakeClient(t, policy, membership, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "replacement-uid"}})
	w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
	assertAuthorizationError(t, w, 403, apierrors.CodeForbidden, "POLICY_DENIED", "operation is not permitted")
}

func testAuthorizationBusinessErrors(t *testing.T) {
	t.Helper()
	for _, tc := range []struct{ route, path, code string }{
		{authorizationClusterRoute, "/api/v1/clusters/unknown", apierrors.CodeClusterNotFound},
		{authorizationCappRoute, "/api/v1/clusters/prod/namespaces/missing/capps/app", apierrors.CodeNotFound},
	} {
		f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
		w := f.serve(t, http.MethodGet, tc.route, tc.path, nil)
		if got := decodeError(t, w); w.Code != http.StatusNotFound || got.Code != tc.code {
			t.Fatalf("business error: status=%d error=%+v, want 404/%s", w.Code, got, tc.code)
		}
	}
}

func testAuthorizationLegacyModes(t *testing.T) {
	t.Helper()
	for _, mode := range []string{"", config.AuthorizationModeKubernetes} {
		f := newAuthorizationFixture(t, nil, "allNamespaces")
		f.cfg.Authorization.Mode = mode
		f.credential = nil
		w := f.serve(t, http.MethodGet, "/legacy", "/legacy", nil)
		if w.Code != http.StatusNoContent || f.state != nil || f.lists != 0 {
			t.Fatalf("legacy mode %q: status=%d state=%+v lists=%d", mode, w.Code, f.state, f.lists)
		}
	}
}

func testAuthorizationInvalidContext(t *testing.T) {
	t.Helper()
	for _, credential := range []any{nil, "untrusted", auth.ClusterCredential{}} {
		f := newAuthorizationFixture(t, nil, "allNamespaces")
		f.credential = credential
		w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
		assertAuthorizationError(t, w, 503, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
	}
	f := newAuthorizationFixture(t, nil, "allNamespaces")
	f.cfg.Authorization.Mode = "unsupported"
	w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
	assertAuthorizationError(t, w, 503, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
}

func TestRespondAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, code, reason, message string
		status                      int
		err                         error
	}{
		{name: "no grant", status: 403, code: apierrors.CodeForbidden, reason: "POLICY_DENIED", message: "operation is not permitted"},
		{name: "policy failure", status: 503, code: apierrors.CodeInternal, reason: "POLICY_UNAVAILABLE", message: "authorization policy is unavailable", err: errors.New("secret diagnostic")},
		{name: "membership conflict", status: 409, code: apierrors.CodeConflict, reason: "POLICY_CONFLICT", message: "membership changed; reload and retry", err: fmt.Errorf("secret diagnostic: %w", authorization.ErrMembershipConflict)},
		{name: "invalid membership", status: 400, code: apierrors.CodeBadRequest, reason: "INVALID_MEMBERSHIP", message: "invalid namespace membership", err: fmt.Errorf("secret diagnostic: %w", authorization.ErrInvalidMembership)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, c := testutil.GinTestContext(t)
			middleware.RespondAuthorization(c, tc.err)
			assertAuthorizationError(t, w, tc.status, tc.code, tc.reason, tc.message)
			if !c.IsAborted() || strings.Contains(c.Errors.String(), "secret diagnostic") {
				t.Fatalf("response: aborted=%t errors=%s, want aborted with sanitized errors", c.IsAborted(), c.Errors.String())
			}
		})
	}
}

func TestRespondProtectedNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, route, scope, clusterName string
		permissions                     []string
	}{
		{
			name: "namespace creation records body namespace", route: authorizationClusterRoute + "/namespaces",
			scope: "cluster", clusterName: "prod", permissions: []string{"namespaces:create"},
		},
		{
			name: "migration records destination target", route: authorizationCappRoute + "/migrate",
			scope: "allNamespaces", clusterName: "destination",
			permissions: []string{"capps:migrate", "capps:get", "secrets:list", "configmaps:list"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthorizationFixture(t, tc.permissions, tc.scope)
			w := f.serve(t, http.MethodPost, tc.route, authorizationPath(tc.route), func(c *gin.Context) {
				middleware.RespondProtectedNamespace(c, tc.clusterName, "credentials")
			})
			assertAuthorizationError(t, w, http.StatusForbidden, apierrors.CodeNamespaceDenied, "PROTECTED_NAMESPACE", "namespace is not accessible")
			if got, want := len(f.state.Decisions), len(tc.permissions)+1; got != want {
				t.Fatalf("decision count = %d, want %d", got, want)
			}
			want := middleware.AuthorizationDecision{
				Cluster: tc.clusterName, Namespace: "credentials", Resource: tc.route, Action: http.MethodPost,
				Outcome: "denied", Reason: "PROTECTED_NAMESPACE",
			}
			if tc.clusterName == "prod" {
				want.PolicyVersion = "policy-version"
			}
			if diff := cmp.Diff(want, f.state.Decisions[len(f.state.Decisions)-1]); diff != "" {
				t.Errorf("protected body target decision (-want +got):\n%s", diff)
			}
		})
	}
}

type authorizationFixture struct {
	cfg         *config.Config
	mgr         *testutil.MockClusterManager
	client      client.Client
	adminClient any
	credential  any
	state       *middleware.AuthorizationState
	called      bool
	lists       int
	gets        int
}

func newAuthorizationFixture(t *testing.T, permissions []string, scope string) *authorizationFixture {
	t.Helper()
	f := &authorizationFixture{
		cfg: &config.Config{
			Authorization: config.AuthorizationConfig{Mode: config.AuthorizationModeApplication},
			Clusters:      []config.ClusterConfig{{Name: "prod", Authorization: config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}}},
			Metrics:       config.MetricsConfig{Enabled: true, Path: "/custom-metrics"},
		},
		credential: auth.ClusterCredential{ImpersonateUser: "alice", ImpersonateGroups: []string{"developers"}},
	}
	f.client = testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			f.lists++
			policies, ok := list.(*corev1.ConfigMapList)
			if !ok {
				t.Fatalf("unexpected list %T; middleware may only list policy", list)
			}
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			policies.ResourceVersion = "policy-version"
			return nil
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			f.gets++
			if _, ok := obj.(*corev1.Namespace); !ok {
				t.Fatalf("unexpected get %T; middleware may only read namespace metadata", obj)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, policyWithPermissions(t, permissions, scope), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "current-uid"}})
	f.mgr = &testutil.MockClusterManager{
		GetFn: func(name string) (*cluster.ClusterClient, error) {
			return &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: name}}, nil
		},
		ClientForFn: func(_ *cluster.ClusterClient, cred auth.ClusterCredential) (client.Client, error) {
			if diff := cmp.Diff(auth.ClusterCredential{}, cred); diff != "" {
				t.Fatalf("policy client credential (-want +got):\n%s", diff)
			}
			return f.client, nil
		},
	}
	return f
}

func (f *authorizationFixture) serve(t *testing.T, method, route, path string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if f.adminClient != nil {
			c.Set(string(middleware.AdminK8sClientKey), f.adminClient)
		}
		if f.credential != nil {
			c.Set(string(middleware.CredentialKey), f.credential)
		}
		c.Next()
		if value, ok := c.Get(string(middleware.AuthorizationKey)); ok {
			f.state, _ = value.(*middleware.AuthorizationState)
		}
	})
	engine.Use(middleware.Authorization(f.cfg, f.mgr))
	if route != "" {
		engine.Handle(method, route, func(c *gin.Context) {
			f.called = true
			if handler != nil {
				handler(c)
			}
			if !c.IsAborted() {
				c.Status(http.StatusNoContent)
			}
		})
	}
	return testutil.ServeHTTP(engine, method, path, nil)
}

func authorizationState(t *testing.T, c *gin.Context) *middleware.AuthorizationState {
	t.Helper()
	value, _ := c.Get(string(middleware.AuthorizationKey))
	state, ok := value.(*middleware.AuthorizationState)
	if !ok {
		t.Fatalf("authorization context = %T, want *AuthorizationState", value)
	}
	return state
}

func authorizationPath(route string) string {
	return strings.NewReplacer(":cluster", "prod", ":namespace", "team", ":name", "app", ":revision", "1").Replace(route)
}

func policyWithPermissions(t *testing.T, permissions []string, scope string) *corev1.ConfigMap {
	t.Helper()
	rules := make([]map[string]any, 0, len(permissions))
	for _, permission := range permissions {
		resource, action, _ := strings.Cut(permission, ":")
		rules = append(rules, map[string]any{"resource": resource, "actions": []string{action}})
	}
	payload, err := json.Marshal(map[string]any{
		"version": 1, "roles": map[string]any{"member": rules},
		"bindings": []map[string]string{{"kind": "User", "name": "alice", "role": "member", "scope": scope}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "capp-policy", Namespace: "policies", Labels: map[string]string{consts.CappAPIGroup + "/authorization": "true"}},
		Data:       map[string]string{"policy.json": string(payload)},
	}
}

func assertAuthorizationError(t *testing.T, w *httptest.ResponseRecorder, status int, code, reason, message string) {
	t.Helper()
	got := decodeError(t, w)
	want := apierrors.APIError{Status: status, Code: code, Message: message, Details: map[string]any{"reason": reason}}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(apierrors.APIError{})); diff != "" || w.Code != status {
		t.Fatalf("authorization response status=%d, want %d (-want +got):\n%s", w.Code, status, diff)
	}
}

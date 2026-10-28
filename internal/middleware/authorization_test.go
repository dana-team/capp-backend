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
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	authorizationClusterRoute   = "/api/v1/clusters/:cluster"
	authorizationNamespaceRoute = authorizationClusterRoute + "/namespaces/:namespace"
	authorizationCappRoute      = authorizationNamespaceRoute + "/capps/:name"
)

func TestAuthorization(t *testing.T) {
	t.Run("last-valid cache during failures", testAuthorizationCacheFailures)
	t.Run("captured generations retire before handler decisions", testAuthorizationCapturedGenerations)
	t.Run("only namespace-dependent requests require namespace availability", func(t *testing.T) { synctest.Test(t, testAuthorizationPolicyOnly) })
	t.Run("requires every permission for each registered operation", testAuthorizationPermissions)
	t.Run("matches method and registered path rather than URL text", func(t *testing.T) { synctest.Test(t, testAuthorizationRouteMatching) })
	t.Run("public exemptions require exact method and path", func(t *testing.T) { synctest.Test(t, testAuthorizationPublicRoutes) })
	t.Run("namespace restrictions override grants before reads", func(t *testing.T) { synctest.Test(t, testAuthorizationNamespaceRestrictions) })
	t.Run("passes snapshots and current metadata to body handlers", func(t *testing.T) { synctest.Test(t, testAuthorizationBodyContext) })
	t.Run("uses the reusable service client", func(t *testing.T) { synctest.Test(t, testAuthorizationServiceClient) })
	t.Run("aggregate handlers receive policy without a blanket grant", func(t *testing.T) { synctest.Test(t, testAuthorizationAggregateContext) })
	t.Run("policy and metadata failures never reach handlers", testAuthorizationFailures)
	t.Run("cluster discovery captures each target and rejects missing stores", func(t *testing.T) { synctest.Test(t, testAuthorizationDiscovery) })
	t.Run("subsequent requests reuse cached policy and preserve verified identity", func(t *testing.T) { synctest.Test(t, testAuthorizationRequestIsolation) })
	t.Run("old namespace memberships cannot authorize a replacement", func(t *testing.T) { synctest.Test(t, testAuthorizationNamespaceIdentity) })
	t.Run("unknown clusters and missing namespaces retain not found responses", func(t *testing.T) { synctest.Test(t, testAuthorizationBusinessErrors) })
	t.Run("legacy modes bypass policy", func(t *testing.T) { synctest.Test(t, testAuthorizationLegacyModes) })
	t.Run("fails closed on missing identity or unsupported mode", func(t *testing.T) { synctest.Test(t, testAuthorizationInvalidContext) })
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
			synctest.Test(t, func(t *testing.T) {
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
		if f.called || f.lists.Load() != 0 || f.gets.Load() != 0 {
			t.Fatalf("unknown route %s %s: handler=%t lists=%d gets=%d, want no execution or reads", tc.method, tc.route, f.called, f.lists.Load(), f.gets.Load())
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
		if w.Code != http.StatusNoContent || f.state != nil || f.lists.Load() != 0 {
			t.Fatalf("public %s %s: status=%d state=%+v lists=%d", tc.method, tc.path, w.Code, f.state, f.lists.Load())
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
		if f.called || f.lists.Load() != 0 || f.gets.Load() != 0 {
			t.Fatalf("restricted %s: handler=%t lists=%d gets=%d, want no execution or reads", namespace, f.called, f.lists.Load(), f.gets.Load())
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
	if w.Code != http.StatusNoContent || f.lists.Load() != 0 || f.gets.Load() != 0 {
		t.Fatalf("body route: status=%d lists=%d gets=%d, want 204/0/0", w.Code, f.lists.Load(), f.gets.Load())
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
	f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
	var acquired bool
	f.mgr.ServiceClientForFn = func(*cluster.ClusterClient) (client.WithWatch, error) {
		acquired = true
		return f.serviceClient, nil
	}
	f.mgr.ClientForFn = func(*cluster.ClusterClient, auth.ClusterCredential) (client.Client, error) {
		t.Fatal("authorization constructed a caller-scoped client")
		return nil, nil
	}
	w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), func(c *gin.Context) {
		if !acquired || authorizationState(t, c).Targets["prod"].Client == nil {
			t.Fatal("handler did not receive the reusable service client")
		}
	})
	if w.Code != http.StatusNoContent || f.lists.Load() != 0 || f.gets.Load() != 0 {
		t.Fatalf("service client reuse: status=%d lists=%d gets=%d, want 204/0/0", w.Code, f.lists.Load(), f.gets.Load())
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
		if w.Code != http.StatusNoContent || f.lists.Load() != 0 || f.gets.Load() != 0 {
			t.Fatalf("aggregate %s: status=%d lists=%d gets=%d, want 204/0/0", route, w.Code, f.lists.Load(), f.gets.Load())
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
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.NamespaceList); ok {
						return errors.New("sensitive namespace read diagnostic")
					}
					return c.List(ctx, list, opts...)
				},
			}, policyWithPermissions(t, []string{"capps:get"}, "allNamespaces"))
		}},
		{name: "evaluation cannot resolve current UID", prepare: func(f *authorizationFixture) {
			f.client = testutil.FakeClient(t, policyWithPermissions(t, []string{"capps:get"}, "allNamespaces"),
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}})
		}},
		{name: "service client failure", prepare: func(f *authorizationFixture) {
			f.mgr.ServiceClientForFn = func(*cluster.ClusterClient) (client.WithWatch, error) {
				return nil, errors.New("sensitive credential diagnostic")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
				tc.prepare(f)
				w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
				assertAuthorizationError(t, w, http.StatusServiceUnavailable, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
				if f.called || len(f.state.Decisions) != 1 || f.state.Decisions[0].Outcome != "unavailable" {
					t.Fatalf("failure: handler=%t decisions=%+v, want one unavailable decision", f.called, f.state.Decisions)
				}
			})
		})
	}
}

func testAuthorizationDiscovery(t *testing.T) {
	t.Helper()
	f := newAuthorizationFixture(t, nil, "allNamespaces")
	f.cfg.Clusters = append(f.cfg.Clusters, config.ClusterConfig{Name: "other", Authorization: f.cfg.Clusters[0].Authorization})
	w := f.serve(t, http.MethodGet, "/api/v1/clusters", "/api/v1/clusters", nil)
	if w.Code != http.StatusNoContent || len(f.state.Targets) != 2 || f.lists.Load() != 0 {
		t.Fatalf("discovery: status=%d targets=%d lists=%d, want 204/2/0", w.Code, len(f.state.Targets), f.lists.Load())
	}
	f.called = false
	delete(f.stores, "other")
	w = f.serve(t, http.MethodGet, "/api/v1/clusters", "/api/v1/clusters", nil)
	assertAuthorizationError(t, w, 503, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
	if f.called || f.state.Decisions[0].Cluster != "other" {
		t.Fatalf("partial discovery: handler=%t decisions=%+v", f.called, f.state.Decisions)
	}
}

func testAuthorizationRequestIsolation(t *testing.T) {
	t.Helper()
	f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
	f.startStores(t)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.CredentialKey), f.credential)
		c.Next()
		f.state = authorizationState(t, c)
	})
	engine.Use(middleware.Authorization(f.cfg, f.mgr, f.stores))
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
	if f.called || f.state.Username != "bob" || previous == f.state.Targets["prod"].Snapshot || f.lists.Load() != 0 {
		t.Fatalf("subsequent request: handler=%t user=%q lists=%d", f.called, f.state.Username, f.lists.Load())
	}
}

func testAuthorizationCacheFailures(t *testing.T) {
	t.Helper()
	for _, failure := range []string{"read", "watch"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
				f.startStores(t)
				f.failReads.Store(true)
				f.mu.Lock()
				policyStream, namespaceStream := f.policyWatch, f.namespaceWatch
				f.mu.Unlock()
				attempts := f.watchAttempts.Load()
				if failure == "watch" {
					f.failWatch.Store(true)
					policyStream.Stop()
					namespaceStream.Stop()
				} else {
					policyStream.Modify(policyWithPermissions(t, nil, "allNamespaces"))
					namespaceStream.Modify(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}})
				}
				synctest.Wait()
				time.Sleep(time.Second)
				synctest.Wait()
				if failure == "watch" && f.watchAttempts.Load() <= attempts {
					t.Fatal("watch reconnect failure was not exercised")
				}
				if failure == "read" && f.lists.Load() == 0 {
					t.Fatal("background read failure was not exercised")
				}
				f.lists.Store(0)
				for range 3 {
					w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
					if w.Code != http.StatusNoContent {
						t.Fatalf("last-valid request during %s failure = %d, want 204", failure, w.Code)
					}
				}
				missing := f.serve(t, http.MethodGet, authorizationCappRoute, "/api/v1/clusters/prod/namespaces/absent/capps/app", nil)
				if missing.Code != http.StatusNotFound || decodeError(t, missing).Code != apierrors.CodeNotFound {
					t.Fatalf("cached miss during %s failure = %d, want NotFound", failure, missing.Code)
				}
				if f.lists.Load() != 0 || f.gets.Load() != 0 {
					t.Fatalf("warm authorization reads = %d/%d, want zero", f.lists.Load(), f.gets.Load())
				}
				f.stores["prod"].Invalidate()
				f.called = false
				w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
				assertAuthorizationError(t, w, 503, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
				if f.called {
					t.Fatal("invalidated policy reached handler")
				}
			})
		})
	}
}

func testAuthorizationCapturedGenerations(t *testing.T) {
	t.Helper()
	for _, retirement := range []string{"policy invalidation", "namespace invalidation", "policy publication", "namespace publication"} {
		t.Run(retirement, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newAuthorizationFixture(t, []string{"capps:get"}, "allNamespaces")
				w := f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), func(c *gin.Context) {
					state := authorizationState(t, c)
					switch retirement {
					case "policy invalidation":
						f.stores["prod"].Invalidate()
					case "namespace invalidation":
						f.stores["prod"].InvalidateNamespaces()
					case "policy publication":
						var policy corev1.ConfigMap
						if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: "policies", Name: "capp-policy"}, &policy); err != nil {
							t.Fatal(err)
						}
						policy.Data = policyWithPermissions(t, nil, "allNamespaces").Data
						if err := f.client.Update(t.Context(), &policy); err != nil {
							t.Fatal(err)
						}
						f.mu.Lock()
						stream := f.policyWatch
						f.mu.Unlock()
						stream.Modify(&policy)
						synctest.Wait()
					case "namespace publication":
						var namespace corev1.Namespace
						if err := f.client.Get(t.Context(), client.ObjectKey{Name: "team"}, &namespace); err != nil {
							t.Fatal(err)
						}
						namespace.Labels = map[string]string{"changed": "true"}
						if err := f.client.Update(t.Context(), &namespace); err != nil {
							t.Fatal(err)
						}
						f.mu.Lock()
						stream := f.namespaceWatch
						f.mu.Unlock()
						stream.Modify(&namespace)
						synctest.Wait()
					}
					allowed, err := state.Targets["prod"].Snapshot.CheckNamespace(state.Namespace, "capps", "get")
					if allowed || !errors.Is(err, authorization.ErrPolicyUnavailable) {
						t.Fatalf("captured check after %s = %v,%v, want unavailable", retirement, allowed, err)
					}
					state.RecordDecision("prod", "team", "capps", "get", allowed, err)
					middleware.RespondAuthorization(c, err)
				})
				assertAuthorizationError(t, w, 503, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
				if len(f.state.Decisions) != 2 || f.state.Decisions[1].Outcome != "unavailable" {
					t.Fatalf("captured decision audit = %+v", f.state.Decisions)
				}
			})
		})
	}
}

func testAuthorizationPolicyOnly(t *testing.T) {
	t.Helper()
	f := newAuthorizationFixture(t, []string{"namespaces:create"}, "cluster")
	f.startStores(t)
	f.failReads.Store(true)
	f.stores["prod"].InvalidateNamespaces()
	synctest.Wait()
	f.lists.Store(0)
	w := f.serve(t, http.MethodPost, authorizationClusterRoute+"/namespaces", "/api/v1/clusters/prod/namespaces", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("policy-only request with unavailable namespaces = %d, want 204", w.Code)
	}
	f.called = false
	w = f.serve(t, http.MethodGet, authorizationCappRoute, authorizationPath(authorizationCappRoute), nil)
	assertAuthorizationError(t, w, 503, apierrors.CodeInternal, "POLICY_UNAVAILABLE", "authorization policy is unavailable")
	if f.called || f.lists.Load() != 0 || f.gets.Load() != 0 {
		t.Fatalf("unavailable namespace request: handler=%v reads=%d/%d", f.called, f.lists.Load(), f.gets.Load())
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
		if w.Code != http.StatusNoContent || f.state != nil || f.lists.Load() != 0 {
			t.Fatalf("legacy mode %q: status=%d state=%+v lists=%d", mode, w.Code, f.state, f.lists.Load())
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
			synctest.Test(t, func(t *testing.T) {
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
		})
	}
}

type authorizationFixture struct {
	cfg            *config.Config
	mgr            *testutil.MockClusterManager
	client         client.Client
	credential     any
	state          *middleware.AuthorizationState
	called         bool
	lists          atomic.Int64
	gets           atomic.Int64
	serviceClient  client.WithWatch
	stores         map[string]*authorization.Store
	started        bool
	failReads      atomic.Bool
	failWatch      atomic.Bool
	watchAttempts  atomic.Int64
	mu             sync.Mutex
	policyWatch    *watch.RaceFreeFakeWatcher
	namespaceWatch *watch.RaceFreeFakeWatcher
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
	f.client = testutil.FakeClient(t, policyWithPermissions(t, permissions, scope), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "current-uid"}})
	f.mgr = &testutil.MockClusterManager{
		GetFn: func(name string) (*cluster.ClusterClient, error) {
			return &cluster.ClusterClient{Meta: cluster.ClusterMeta{Name: name}}, nil
		},
		ServiceClientForFn: func(*cluster.ClusterClient) (client.WithWatch, error) {
			return f.serviceClient, nil
		},
	}
	return f
}

// startStores models server-owned startup before requests are served.
func (f *authorizationFixture) startStores(t *testing.T) {
	t.Helper()
	if f.started {
		return
	}
	f.started = true
	target, ok := f.client.(client.WithWatch)
	if !ok {
		t.Fatal("fixture client must support watches")
	}
	f.serviceClient = interceptor.NewClient(target, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			f.lists.Add(1)
			if f.failReads.Load() {
				return errors.New("offline")
			}
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			switch list := list.(type) {
			case *corev1.ConfigMapList:
				list.ResourceVersion = "policy-version"
			case *corev1.NamespaceList:
				list.ResourceVersion = "namespace-version"
			}
			return nil
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			f.gets.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
		Watch: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
			f.watchAttempts.Add(1)
			if f.failWatch.Load() {
				return nil, errors.New("offline")
			}
			stream := watch.NewRaceFreeFake()
			f.mu.Lock()
			defer f.mu.Unlock()
			switch list.(type) {
			case *corev1.ConfigMapList:
				f.policyWatch = stream
			case *corev1.NamespaceList:
				f.namespaceWatch = stream
			}
			return stream, nil
		},
	})
	f.stores = make(map[string]*authorization.Store)
	for _, cfg := range f.cfg.Clusters {
		store, err := authorization.NewStore(f.serviceClient, cfg.Authorization, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(store.Stop)
		if err := store.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.stores[cfg.Name] = store
	}
	synctest.Wait()
	time.Sleep(time.Second)
	synctest.Wait()
	f.lists.Store(0)
	f.gets.Store(0)
}

func (f *authorizationFixture) serve(t *testing.T, method, route, path string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	f.startStores(t)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if f.credential != nil {
			c.Set(string(middleware.CredentialKey), f.credential)
		}
		c.Next()
		if value, ok := c.Get(string(middleware.AuthorizationKey)); ok {
			f.state, _ = value.(*middleware.AuthorizationState)
		}
	})
	engine.Use(middleware.Authorization(f.cfg, f.mgr, f.stores))
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

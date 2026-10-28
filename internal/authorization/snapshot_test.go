package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/casbin/casbin/v3"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestLoad(t *testing.T) {
	t.Run("one authoritative list retains versions and serves repeated checks", func(t *testing.T) {
		objects := snapshotObjects(t)
		objects = append(objects,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "policies"}, Data: map[string]string{policyDataKey: "invalid"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterPolicyName, Namespace: "other", Labels: map[string]string{policyLabelKey: "true"}}},
		)
		ctx := t.Context()
		reads := 0
		target := snapshotClient(t, interceptor.Funcs{
			List: func(gotCtx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				reads++
				if gotCtx != ctx {
					t.Fatal("Load did not propagate the request context")
				}
				options := (&client.ListOptions{}).ApplyOptions(opts)
				raw := options.AsListOptions()
				if options.Namespace != "policies" || raw.LabelSelector != policyLabelKey+"=true" || raw.ResourceVersion != "" || raw.Limit != 0 || raw.Continue != "" {
					t.Fatalf("Load LIST options = %+v, want namespace/label filter and default authoritative unpaginated read", options)
				}
				if err := c.List(gotCtx, list, opts...); err != nil {
					return err
				}
				list.SetResourceVersion("42")
				return nil
			},
		}, objects...)
		snapshot, err := Load(ctx, target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if got := snapshot.ResourceVersion(); got != "42" {
			t.Errorf("ResourceVersion() = %q, want 42", got)
		}
		wantVersions := map[string]string{clusterPolicyName: "10", membershipNamePrefix + "uid-1": "20"}
		if diff := cmp.Diff(wantVersions, snapshot.resourceVersions); diff != "" {
			t.Errorf("record versions mismatch (-want +got):\n%s", diff)
		}
		for range 2 {
			allowed, err := snapshot.CheckNamespace(currentNamespace(), "capps", "get")
			if err != nil || !allowed {
				t.Fatalf("CheckNamespace(capps, get) = %v, %v, want true, nil", allowed, err)
			}
		}
		if reads != 1 {
			t.Errorf("policy reads = %d, want 1", reads)
		}
	})

	for _, tc := range []struct {
		name   string
		modify func([]*corev1.ConfigMap) []*corev1.ConfigMap
		want   string
	}{
		{name: "missing policy", modify: func(objects []*corev1.ConfigMap) []*corev1.ConfigMap { return objects[1:] }, want: "missing cluster policy"},
		{name: "malformed policy", modify: func(objects []*corev1.ConfigMap) []*corev1.ConfigMap {
			objects[0].Data[policyDataKey] = "invalid"
			return objects
		}, want: "invalid policy JSON"},
		{name: "unknown permission", modify: func(objects []*corev1.ConfigMap) []*corev1.ConfigMap {
			record := objects[0]
			record.Data[policyDataKey] = strings.ReplaceAll(record.Data[policyDataKey], `"get"`, `"unknown"`)
			return objects
		}, want: "unsupported resource or action"},
		{name: "invalid binding in unrelated namespace", modify: func(objects []*corev1.ConfigMap) []*corev1.ConfigMap {
			other := objects[1].DeepCopy()
			other.Name = membershipNamePrefix + "uid-2"
			other.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "other", UID: "uid-2"}}
			other.Data[policyDataKey] = `{"version":1,"namespace":{"name":"other","uid":"uid-2"},"users":[],"bindings":[{"kind":"User","name":"bob","role":"undefined"}]}`
			return append(objects, other)
		}, want: "undefined role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := snapshotClient(t, interceptor.Funcs{}, tc.modify(snapshotObjects(t))...)
			snapshot, err := Load(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: "alice"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || snapshot != nil {
				t.Fatalf("Load() = %v, %v, want nil snapshot and %q error", snapshot, err, tc.want)
			}
		})
	}

	t.Run("read failure is distinct from denial", func(t *testing.T) {
		unavailable := errors.New("API server unavailable")
		target := snapshotClient(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return unavailable
		}})
		snapshot, err := Load(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: "alice"})
		if !errors.Is(err, unavailable) || snapshot != nil {
			t.Fatalf("Load() = %v, %v, want nil snapshot and API error", snapshot, err)
		}
	})

	t.Run("incomplete list cannot authorize", func(t *testing.T) {
		target := snapshotClient(t, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			list.SetContinue("next-page")
			return nil
		}}, snapshotObjects(t)...)
		snapshot, err := Load(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: "alice"})
		if err == nil || !strings.Contains(err.Error(), "incomplete") || snapshot != nil {
			t.Fatalf("Load() = %v, %v, want nil snapshot and incomplete-list error", snapshot, err)
		}
	})

	t.Run("cancelled request performs no read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		target := snapshotClient(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Fatal("cancelled Load issued a policy read")
			return nil
		}})
		_, err := Load(ctx, target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: "alice"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Load() error = %v, want context.Canceled", err)
		}
	})

	for _, tc := range []struct {
		name       string
		cfg        config.ClusterAuthorizationConfig
		credential auth.ClusterCredential
	}{
		{name: "missing username", cfg: config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}},
		{name: "missing namespace", credential: auth.ClusterCredential{ImpersonateUser: "alice"}},
		{name: "blank group", cfg: config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, credential: auth.ClusterCredential{ImpersonateUser: "alice", ImpersonateGroups: []string{" "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := snapshotClient(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				t.Fatal("invalid input caused a policy read")
				return nil
			}})
			if snapshot, err := Load(t.Context(), target, tc.cfg, tc.credential); err == nil || snapshot != nil {
				t.Fatalf("Load() = %v, %v, want nil snapshot and validation error", snapshot, err)
			}
		})
	}
}

func TestSnapshotCheckNamespace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		user      string
		groups    []string
		resource  string
		action    string
		namespace *corev1.Namespace
		want      bool
	}{
		{name: "direct member", user: "alice", resource: "capps", action: "get", want: true},
		{name: "group supplies an additional permission", user: "alice", groups: []string{"team-editors"}, resource: "capps", action: "update", want: true},
		{name: "unmatched groups do not remove user grants", user: "alice", groups: []string{"unknown"}, resource: "capps", action: "get", want: true},
		{name: "unbound identity", user: "bob", resource: "capps", action: "get"},
		{name: "user cannot become a same-name group", user: "team-editors", resource: "capps", action: "update"},
		{name: "user cannot become a same-name role", user: "capp-user", resource: "capps", action: "get"},
		{name: "canonical username is case sensitive", user: "Alice", resource: "capps", action: "get"},
		{name: "implicit groups are not synthesized", user: "bob", resource: "secrets", action: "list"},
		{name: "grant does not imply replace", user: "alice", resource: "roles/capp-user", action: "replace"},
		{name: "explicit invitation", user: "alice", resource: "roles/capp-user", action: "grant", want: true},
		{name: "operator delegation", user: "inviter", resource: "roles/capp-user", action: "replace", want: true},
		{name: "cluster grant cannot authorize namespace operation", user: "admin", resource: "namespaces", action: "create"},
		{name: "membership does not cross namespaces", user: "alice", resource: "capps", action: "get", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "uid-2"}}},
		{name: "replacement namespace does not inherit membership", user: "alice", resource: "capps", action: "get", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "replacement"}}},
		{name: "global group grant needs no membership record", user: "bob", groups: []string{"global-readers"}, resource: "secrets", action: "get", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "uid-2"}}, want: true},
		{name: "shared role permissions reach every binding", user: "global-reader", resource: "secrets", action: "get", want: true},
		{name: "unknown requested action", user: "alice", resource: "capps", action: "*"},
		{name: "unknown requested resource", user: "alice", resource: "pods", action: "get"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := loadSnapshot(t, auth.ClusterCredential{ImpersonateUser: tc.user, ImpersonateGroups: tc.groups})
			namespace := tc.namespace
			if namespace == nil {
				namespace = currentNamespace()
			}
			got, err := snapshot.CheckNamespace(namespace, tc.resource, tc.action)
			if err != nil || got != tc.want {
				t.Errorf("CheckNamespace(%s, %s, %s) = %v, %v, want %v, nil", namespace.Name, tc.resource, tc.action, got, err, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name      string
		namespace *corev1.Namespace
	}{
		{name: "missing metadata"},
		{name: "missing UID", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}},
		{name: "policy name disagrees with current UID", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "uid-1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := loadSnapshot(t, auth.ClusterCredential{ImpersonateUser: "alice"})
			if allowed, err := snapshot.CheckNamespace(tc.namespace, "capps", "get"); allowed || err == nil {
				t.Errorf("CheckNamespace(%v) = %v, %v, want false and metadata error", tc.namespace, allowed, err)
			}
		})
	}
}

func TestSnapshotCheckCluster(t *testing.T) {
	for _, tc := range []struct {
		name     string
		user     string
		resource string
		action   string
		want     bool
	}{
		{name: "explicit cluster grant", user: "admin", resource: "namespaces", action: "create", want: true},
		{name: "namespace grant cannot authorize cluster operation", user: "alice", resource: "capps", action: "get"},
		{name: "allNamespaces excludes cluster domain", user: "global-reader", resource: "secrets", action: "get"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := loadSnapshot(t, auth.ClusterCredential{ImpersonateUser: tc.user})
			if got, err := snapshot.CheckCluster(tc.resource, tc.action); got != tc.want || err != nil {
				t.Errorf("CheckCluster(%s, %s) = %v, %v, want %v, nil", tc.resource, tc.action, got, err, tc.want)
			}
		})
	}
}

func TestSnapshotCheckAllNamespaces(t *testing.T) {
	for _, tc := range []struct {
		user string
		want bool
	}{
		{user: "admin", want: true},
		{user: "alice"},
	} {
		t.Run(tc.user, func(t *testing.T) {
			snapshot := loadSnapshot(t, auth.ClusterCredential{ImpersonateUser: tc.user})
			if got, err := snapshot.CheckAllNamespaces("roles/capp-user", "grant"); got != tc.want || err != nil {
				t.Errorf("CheckAllNamespaces(grant) for %s = %v, %v, want %v, nil", tc.user, got, err, tc.want)
			}
		})
	}
}

func TestSnapshotCheck(t *testing.T) {
	t.Run("delimiter-containing names remain distinct", func(t *testing.T) {
		objects := snapshotObjects(t)[:1]
		objects[0].Data[policyDataKey] = `{
			"version":1,
			"roles":{
				"x,Role/y":[{"resource":"secrets","actions":["get"]}],
				"y":[{"resource":"capps","actions":["get"]}]
			},
			"bindings":[
				{"kind":"User","name":"a","role":"x,Role/y","scope":"allNamespaces"},
				{"kind":"User","name":"a,Role/x","role":"y","scope":"allNamespaces"}
			]
		}`
		target := snapshotClient(t, interceptor.Funcs{}, objects...)
		for _, tc := range []struct {
			user            string
			allowedResource string
			deniedResource  string
		}{
			{user: "a", allowedResource: "secrets", deniedResource: "capps"},
			{user: "a,Role/x", allowedResource: "capps", deniedResource: "secrets"},
		} {
			snapshot, err := Load(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: tc.user})
			if err != nil {
				t.Fatal(err)
			}
			if allowed, err := snapshot.CheckNamespace(currentNamespace(), tc.allowedResource, "get"); !allowed || err != nil {
				t.Errorf("CheckNamespace(%s) for %q = %v, %v, want true, nil", tc.allowedResource, tc.user, allowed, err)
			}
			if allowed, err := snapshot.CheckNamespace(currentNamespace(), tc.deniedResource, "get"); allowed || err != nil {
				t.Errorf("CheckNamespace(%s) for %q = %v, %v, want false, nil", tc.deniedResource, tc.user, allowed, err)
			}
		}
	})

	t.Run("evaluation error overrides an earlier subject grant", func(t *testing.T) {
		snapshot := loadSnapshot(t, auth.ClusterCredential{ImpersonateUser: "alice", ImpersonateGroups: []string{"team-editors"}})
		var evaluated []any
		// Inject an evaluator failure without introducing a production test interface.
		enforcer := snapshot.evaluators.namespaces[currentNamespace().UID]
		enforcer.GetModel()["m"]["m"].Value = "subjectCheck(r_sub)"
		enforcer.AddFunction("subjectCheck", func(args ...any) (any, error) {
			evaluated = append(evaluated, args[0])
			if args[0] == policyIdentifier("User", "alice") {
				return true, nil
			}
			return false, errors.New("private policy diagnostics")
		})
		allowed, err := snapshot.CheckNamespace(currentNamespace(), "capps", "get")
		if allowed || err == nil || err.Error() != "authorization policy evaluation failed" {
			t.Fatalf("CheckNamespace() = %v, %v, want false and sanitized evaluation error", allowed, err)
		}
		want := []any{policyIdentifier("User", "alice"), policyIdentifier("Group", "team-editors")}
		if diff := cmp.Diff(want, evaluated); diff != "" {
			t.Errorf("evaluated subjects mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("empty policy denies access", func(t *testing.T) {
		objects := snapshotObjects(t)[:1]
		objects[0].Data[policyDataKey] = `{"version":1,"roles":{},"bindings":[]}`
		target := snapshotClient(t, interceptor.Funcs{}, objects...)
		snapshot, err := Load(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if allowed, err := snapshot.CheckCluster("clusters", "get"); allowed || err != nil {
			t.Errorf("CheckCluster() = %v, %v, want false, nil", allowed, err)
		}
	})
}

func TestSnapshot(t *testing.T) {
	objects := snapshotObjects(t)
	target := snapshotClient(t, interceptor.Funcs{}, objects...)
	credential := auth.ClusterCredential{ImpersonateUser: "bob", ImpersonateGroups: []string{"global-readers"}}
	cfg := config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}
	first, err := Load(t.Context(), target, cfg, credential)
	if err != nil {
		t.Fatal(err)
	}
	credential.ImpersonateGroups[0] = "unknown"
	second, err := Load(t.Context(), target, cfg, credential)
	if err != nil {
		t.Fatal(err)
	}
	// A separately loaded target with no bindings must not reuse the first enforcer.
	objects[0].Data[policyDataKey] = `{"version":1,"roles":{"capp-user":[],"editor":[],"reader":[],"delegator":[]},"bindings":[]}`
	other, err := Load(t.Context(), snapshotClient(t, interceptor.Funcs{}, objects...), cfg, auth.ClusterCredential{ImpersonateUser: "bob", ImpersonateGroups: []string{"global-readers"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		snapshot *Snapshot
		want     bool
	}{
		{name: "original identity", snapshot: first, want: true},
		{name: "different caller groups", snapshot: second},
		{name: "different target policy", snapshot: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.snapshot.CheckNamespace(currentNamespace(), "secrets", "get"); got != tc.want || err != nil {
				t.Errorf("CheckNamespace() = %v, %v, want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestCompilePolicyEvaluators(t *testing.T) {
	objects := snapshotObjects(t)
	items := make([]corev1.ConfigMap, len(objects))
	for i, object := range objects {
		items[i] = *object
	}
	records, err := parsePolicyRecords(items, "policies")
	if err != nil {
		t.Fatal(err)
	}
	records.memberships["uid-2"] = membershipPolicy{
		Version: 1, Namespace: namespaceIdentity{Name: "other", UID: "uid-2"},
		Users: []string{"bob"}, Bindings: []binding{{Kind: "User", Name: "alice", Role: "reader"}},
	}
	evaluators, err := compilePolicyEvaluators(records)
	if err != nil {
		t.Fatal(err)
	}
	// The unpartitioned evaluator is the reference for the existing permission semantics.
	full, err := compilePolicy(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluators.namespaces) != len(records.memberships) {
		t.Fatalf("namespace evaluators = %d, want %d", len(evaluators.namespaces), len(records.memberships))
	}
	namespaceCallers := []auth.ClusterCredential{
		{ImpersonateUser: "alice", ImpersonateGroups: []string{"team-editors"}},
		{ImpersonateUser: "bob"},
	}
	allCallers := slices.Concat(namespaceCallers, []auth.ClusterCredential{
		{ImpersonateUser: "global-reader"},
		{ImpersonateUser: "outsider", ImpersonateGroups: []string{"global-readers"}},
		{ImpersonateUser: "admin"},
	})
	for _, scope := range []struct {
		domain   string
		enforcer *casbin.SyncedEnforcer
		domains  []string
		callers  []auth.ClusterCredential
	}{
		{domain: clusterScope, enforcer: evaluators.cluster, domains: []string{clusterScope}, callers: allCallers},
		{domain: allNamespacesScope, enforcer: evaluators.allNamespaces, domains: []string{allNamespacesScope}, callers: allCallers},
		{domain: "ns/uid-1", enforcer: evaluators.namespaces["uid-1"], domains: []string{"ns/uid-1", allNamespacesScope}, callers: allCallers},
		{domain: "ns/uid-2", enforcer: evaluators.namespaces["uid-2"], domains: []string{"ns/uid-2", allNamespacesScope}, callers: namespaceCallers},
		{domain: "ns/replacement", enforcer: evaluators.allNamespaces, domains: []string{allNamespacesScope}, callers: allCallers},
	} {
		t.Run(scope.domain, func(t *testing.T) {
			t.Parallel()
			if scope.enforcer == nil {
				t.Fatal("missing scoped evaluator")
			}
			for _, policy := range []struct {
				name        string
				domainIndex int
			}{
				{name: "p", domainIndex: 1},
				{name: "g", domainIndex: 2},
			} {
				var want [][]string
				for _, row := range full.GetModel()[policy.name][policy.name].Policy {
					if slices.Contains(scope.domains, row[policy.domainIndex]) {
						want = append(want, row)
					}
				}
				got := scope.enforcer.GetModel()[policy.name][policy.name].Policy
				if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b []string) bool { return slices.Compare(a, b) < 0 })); diff != "" {
					t.Errorf("%s policy rows mismatch (-want +got):\n%s", policy.name, diff)
				}
			}
			for _, caller := range scope.callers {
				t.Run(caller.ImpersonateUser, func(t *testing.T) {
					// All callers and scopes share the same immutable evaluator set.
					t.Parallel()
					subjects, err := verifiedSubjects(caller)
					if err != nil {
						t.Fatal(err)
					}
					snapshot := &Snapshot{evaluators: evaluators, subjects: subjects, records: records}
					for _, permission := range [][2]string{
						{"capps", "get"}, {"capps", "update"}, {"secrets", "get"},
						{"namespaces", "create"}, {"roles/capp-user", "replace"},
						{"pods", "get"}, {"capps", "*"},
					} {
						want := false
						for _, subject := range subjects {
							allowed, err := full.Enforce(subject, scope.domain, permission[0], permission[1])
							if err != nil {
								t.Fatal(err)
							}
							want = want || allowed
						}
						for range 20 {
							got, err := snapshot.check(scope.domain, permission[0], permission[1])
							if err != nil || got != want {
								t.Fatalf("check(%s, %s) = %v, %v, want %v, nil", permission[0], permission[1], got, err, want)
							}
						}
					}
				})
			}
		})
	}
}

func loadSnapshot(t *testing.T, credential auth.ClusterCredential) *Snapshot {
	t.Helper()
	snapshot, err := Load(t.Context(), snapshotClient(t, interceptor.Funcs{}, snapshotObjects(t)...), config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, credential)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func currentNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid-1"}}
}

func snapshotClient(t *testing.T, funcs interceptor.Funcs, records ...*corev1.ConfigMap) client.WithWatch {
	t.Helper()
	// Avoid coupling authorization tests to middleware through testutil.
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, len(records))
	for i := range records {
		objects[i] = records[i]
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(funcs).Build()
}

func snapshotObjects(t *testing.T) []*corev1.ConfigMap {
	t.Helper()
	policy := clusterPolicy{
		Version: 1,
		Roles: map[string][]permission{
			"capp-user": {{Resource: "capps", Actions: []string{"get", "sync"}}, {Resource: "roles/capp-user", Actions: []string{"grant"}}},
			"reader":    {{Resource: "secrets", Actions: []string{"get", "list"}}},
			"editor":    {{Resource: "capps", Actions: []string{"update"}}},
			"creator":   {{Resource: "namespaces", Actions: []string{"create"}}},
			"delegator": {{Resource: "roles/capp-user", Actions: []string{"grant", "replace"}}},
		},
		Bindings: []clusterBinding{
			{binding: binding{Kind: "Group", Name: "global-readers", Role: "reader"}, Scope: allNamespacesScope},
			{binding: binding{Kind: "User", Name: "global-reader", Role: "reader"}, Scope: allNamespacesScope},
			{binding: binding{Kind: "User", Name: "admin", Role: "creator"}, Scope: clusterScope},
			{binding: binding{Kind: "User", Name: "admin", Role: "delegator"}, Scope: allNamespacesScope},
			{binding: binding{Kind: "Group", Name: "system:authenticated", Role: "reader"}, Scope: allNamespacesScope},
		},
	}
	membership := membershipPolicy{
		Version: 1, Namespace: namespaceIdentity{Name: "team", UID: "uid-1"}, Users: []string{"alice"},
		Bindings: []binding{{Kind: "Group", Name: "team-editors", Role: "editor"}, {Kind: "User", Name: "inviter", Role: "delegator"}},
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	membershipJSON, err := json.Marshal(membership)
	if err != nil {
		t.Fatal(err)
	}
	return []*corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{Name: clusterPolicyName, Namespace: "policies", ResourceVersion: "10", Labels: map[string]string{policyLabelKey: "true"}},
			Data:       map[string]string{policyDataKey: string(policyJSON)},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: membershipNamePrefix + "uid-1", Namespace: "policies", ResourceVersion: "20", Labels: map[string]string{policyLabelKey: "true"},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "team", UID: "uid-1"}},
			},
			Data: map[string]string{policyDataKey: string(membershipJSON)},
		},
	}
}

package authorization

import (
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParsePolicyRecords(t *testing.T) {
	const (
		clusterJSON    = `{"version":1,"roles":{"capp-user":[{"resource":"namespaces","actions":["get"]}]},"bindings":[{"kind":"Group","name":" Devs ","role":"capp-user","scope":"allNamespaces"}]}`
		membershipJSON = `{"version":1,"namespace":{"name":"team","uid":"uid-1"},"users":[" Alice ","alice"],"bindings":[{"kind":"User","name":" Devs ","role":"capp-user"}]}`
	)
	records := func() []corev1.ConfigMap {
		return []corev1.ConfigMap{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "capp-policy", Namespace: "policies",
					Labels: map[string]string{"rcs.dana.io/authorization": "true"},
				},
				Data: map[string]string{"policy.json": clusterJSON},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "capp-members-uid-1", Namespace: "policies",
					Labels:          map[string]string{"rcs.dana.io/authorization": "true"},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "team", UID: "uid-1"}},
				},
				Data: map[string]string{"policy.json": membershipJSON},
			},
		}
	}

	t.Run("preserves canonical identities and accepts membership before roles", func(t *testing.T) {
		input := records()
		got, err := parsePolicyRecords([]corev1.ConfigMap{input[1], input[0]}, "policies")
		if err != nil {
			t.Fatal(err)
		}
		wantCluster := clusterPolicy{
			Version:  1,
			Roles:    map[string][]permission{"capp-user": {{Resource: "namespaces", Actions: []string{"get"}}}},
			Bindings: []clusterBinding{{binding: binding{Kind: "Group", Name: " Devs ", Role: "capp-user"}, Scope: "allNamespaces"}},
		}
		wantMembership := membershipPolicy{
			Version: 1, Namespace: namespaceIdentity{Name: "team", UID: "uid-1"},
			Users:    []string{" Alice ", "alice"},
			Bindings: []binding{{Kind: "User", Name: " Devs ", Role: "capp-user"}},
		}
		if diff := cmp.Diff(wantCluster, got.cluster, cmp.AllowUnexported(clusterBinding{})); diff != "" {
			t.Errorf("cluster policy mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(wantMembership, got.memberships["uid-1"]); diff != "" {
			t.Errorf("membership mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("accepts a different configured policy namespace", func(t *testing.T) {
		input := records()
		for i := range input {
			input[i].Namespace = "authorization-system"
		}
		if _, err := parsePolicyRecords(input, "authorization-system"); err != nil {
			t.Fatalf("parsePolicyRecords with matching configured namespace: error = %v, want nil", err)
		}
	})

	t.Run("empty grants and absent membership are valid", func(t *testing.T) {
		input := records()[:1]
		input[0].Data[policyDataKey] = `{"version":1,"roles":{},"bindings":[]}`
		got, err := parsePolicyRecords(input, "policies")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.cluster.Roles) != 0 || len(got.cluster.Bindings) != 0 || len(got.memberships) != 0 {
			t.Fatalf("expected no grants, got %+v", got)
		}
	})

	t.Run("empty membership does not require member role", func(t *testing.T) {
		input := records()
		input[0].Data[policyDataKey] = `{"version":1,"roles":{},"bindings":[]}`
		input[1].Data[policyDataKey] = `{"version":1,"namespace":{"name":"team","uid":"uid-1"},"users":[],"bindings":[]}`
		input[1].OwnerReferences[0].BlockOwnerDeletion = new(false)
		if _, err := parsePolicyRecords(input, "policies"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cluster scope and empty role permissions are valid", func(t *testing.T) {
		input := records()
		input[0].Data[policyDataKey] = `{"version":1,"roles":{"capp-user":[]},"bindings":[{"kind":"User","name":"admin","role":"capp-user","scope":"cluster"}]}`
		if _, err := parsePolicyRecords(input, "policies"); err != nil {
			t.Fatal(err)
		}
	})

	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "record at size limit", size: 512 * 1024},
		{name: "record over size limit", size: 512*1024 + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := records()
			input[1].Data[policyDataKey] = membershipJSON + strings.Repeat(" ", tc.size-len(membershipJSON))
			_, err := parsePolicyRecords(input, "policies")
			if (err != nil) != tc.wantErr {
				t.Fatalf("size %d: error = %v, want error %v", tc.size, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "512 KiB") {
				t.Fatalf("size %d: unexpected error: %v", tc.size, err)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		index   int
		payload string
		want    string
	}{
		{name: "malformed JSON", payload: `{`, want: "invalid policy JSON"},
		{name: "wrong field type", payload: `{"version":"private-value"}`, want: "invalid policy JSON"},
		{name: "null record", payload: `null`, want: "requires version 1, roles and bindings"},
		{name: "unknown version", payload: strings.Replace(clusterJSON, `"version":1`, `"version":2`, 1), want: "requires version 1"},
		{name: "missing roles", payload: `{"version":1,"bindings":[]}`, want: "roles and bindings"},
		{name: "null bindings", payload: `{"version":1,"roles":{},"bindings":null}`, want: "roles and bindings"},
		{name: "duplicate version", payload: strings.Replace(clusterJSON, `"version":1`, `"version":1,"version":1`, 1), want: "duplicate or unknown"},
		{name: "duplicate role", payload: `{"version":1,"roles":{"capp-user":[],"capp-user":[]},"bindings":[]}`, want: "duplicate or unknown"},
		{name: "custom matcher", payload: strings.Replace(clusterJSON, `"version":1`, `"version":1,"matcher":"allow all"`, 1), want: "duplicate or unknown"},
		{name: "unknown permission field", payload: strings.Replace(clusterJSON, `"resource":"namespaces"`, `"resource":"namespaces","name":"team"`, 1), want: "duplicate or unknown"},
		{name: "blank role name", payload: `{"version":1,"roles":{" ":[]},"bindings":[]}`, want: "role requires a name"},
		{name: "null permissions", payload: `{"version":1,"roles":{"capp-user":null},"bindings":[]}`, want: "permissions array"},
		{name: "empty actions", payload: strings.Replace(clusterJSON, `["get"]`, `[]`, 1), want: "permission requires actions"},
		{name: "unknown resource", payload: strings.Replace(clusterJSON, `"namespaces"`, `"pods"`, 1), want: "unsupported resource or action"},
		{name: "unknown action", payload: strings.Replace(clusterJSON, `"get"`, `"*"`, 1), want: "unsupported resource or action"},
		{name: "unknown scope", payload: strings.Replace(clusterJSON, `"allNamespaces"`, `"*"`, 1), want: "unsupported binding scope"},
		{name: "unsupported subject kind", payload: strings.Replace(clusterJSON, `"Group"`, `"ServiceAccount"`, 1), want: "unsupported binding kind"},
		{name: "blank subject name", payload: strings.Replace(clusterJSON, `" Devs "`, `" "`, 1), want: "binding requires a name and role"},
		{name: "blank binding role", payload: strings.Replace(clusterJSON, `"role":"capp-user"`, `"role":" "`, 1), want: "binding requires a name and role"},
		{name: "undefined cluster role", payload: strings.Replace(clusterJSON, `"role":"capp-user"`, `"role":"missing"`, 1), want: "undefined role"},
		{name: "undefined member role", payload: `{"version":1,"roles":{},"bindings":[]}`, want: "undefined capp-user role"},
		{name: "missing membership version", index: 1, payload: strings.Replace(membershipJSON, `"version":1,`, ``, 1), want: "membership requires version 1"},
		{name: "missing users", index: 1, payload: strings.Replace(membershipJSON, `"users":[" Alice ","alice"],`, ``, 1), want: "users and bindings"},
		{name: "null namespace bindings", index: 1, payload: strings.Replace(membershipJSON, `[{"kind":"User","name":" Devs ","role":"capp-user"}]`, `null`, 1), want: "users and bindings"},
		{name: "missing namespace identity", index: 1, payload: `{"version":1,"users":[],"bindings":[]}`, want: "invalid membership namespace identity"},
		{name: "invalid namespace name", index: 1, payload: strings.Replace(membershipJSON, `"team"`, `"Team"`, 1), want: "invalid membership namespace identity"},
		{name: "blank namespace UID", index: 1, payload: strings.Replace(membershipJSON, `"uid-1"`, `" "`, 1), want: "invalid membership namespace identity"},
		{name: "blank direct user", index: 1, payload: strings.Replace(membershipJSON, `" Alice "`, `" "`, 1), want: "blank membership user"},
		{name: "duplicate direct user", index: 1, payload: strings.Replace(membershipJSON, `" Alice "`, `"alice"`, 1), want: "duplicate membership user"},
		{name: "undefined operator role", index: 1, payload: strings.Replace(membershipJSON, `"role":"capp-user"`, `"role":"missing"`, 1), want: "undefined role"},
		{name: "namespace binding cannot specify scope", index: 1, payload: strings.Replace(membershipJSON, `"kind":"User"`, `"kind":"User","scope":"cluster"`, 1), want: "duplicate or unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := records()
			input[tc.index].Data[policyDataKey] = tc.payload
			got, err := parsePolicyRecords(input, "policies")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
			if got != nil {
				t.Fatal("invalid policy returned usable records")
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("error leaks policy value")
			}
		})
	}

	for _, tc := range []struct {
		name   string
		mutate func([]corev1.ConfigMap) []corev1.ConfigMap
		want   string
	}{
		{name: "missing cluster policy", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { return r[1:] }, want: "missing cluster policy"},
		{name: "duplicate cluster record", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { return append(r, r[0]) }, want: "duplicate policy record"},
		{name: "wrong policy namespace", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[0].Namespace = "other"; return r }, want: "outside policy namespace"},
		{name: "missing authorization label", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[0].Labels = nil; return r }, want: "missing authorization label"},
		{name: "disabled authorization label", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[0].Labels[policyLabelKey] = "false"; return r }, want: "missing authorization label"},
		{name: "managed-resource label", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap {
			r[0].Labels[consts.ManagedLabelKey] = consts.ManagedLabelValue
			return r
		}, want: "must not have managed-resource label"},
		{name: "invalid ConfigMap name", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[1].Name = "INVALID"; return r }, want: "invalid policy record name"},
		{name: "missing JSON data", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[0].Data = nil; return r }, want: "missing policy.json data"},
		{name: "cluster policy namespace owner", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[0].OwnerReferences = r[1].OwnerReferences; return r }, want: "must not have a namespace owner"},
		{name: "membership record UID mismatch", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[1].Name = "capp-members-uid-2"; return r }, want: "name does not match namespace UID"},
		{name: "missing namespace owner", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[1].OwnerReferences = nil; return r }, want: "missing membership namespace owner"},
		{name: "wrong owner kind", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[1].OwnerReferences[0].Kind = "ConfigMap"; return r }, want: "missing membership namespace owner"},
		{name: "wrong owner API version", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap {
			r[1].OwnerReferences[0].APIVersion = "custom/v1"
			return r
		}, want: "owner does not match identity"},
		{name: "owner name mismatch", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[1].OwnerReferences[0].Name = "other"; return r }, want: "owner does not match identity"},
		{name: "owner UID mismatch", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap { r[1].OwnerReferences[0].UID = "uid-2"; return r }, want: "owner does not match identity"},
		{name: "duplicate namespace owner", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap {
			r[1].OwnerReferences = append(r[1].OwnerReferences, r[1].OwnerReferences[0])
			return r
		}, want: "owner does not match identity"},
		{name: "blocking namespace deletion", mutate: func(r []corev1.ConfigMap) []corev1.ConfigMap {
			r[1].OwnerReferences[0].BlockOwnerDeletion = new(true)
			return r
		}, want: "must not block namespace deletion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePolicyRecords(tc.mutate(records()), "policies")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestKnownPermission(t *testing.T) {
	allowed := map[string][]string{
		"clusters":        {"get"},
		"namespaces":      {"get", "create", "update", "delete"},
		"capps":           {"list", "get", "create", "update", "delete", "sync", "unsync", "migrate"},
		"secrets":         {"list", "get", "create", "update", "delete"},
		"configmaps":      {"list", "get", "create", "update", "delete"},
		"capprevisions":   {"list", "get"},
		"benchmarks":      {"get", "create"},
		"roles/capp-user": {"grant", "replace"},
	}
	// Each resource rejects actions belonging to other resources, including rollback
	// (which is authorized as revision read and Capp read/update, not a new action).
	actions := []string{"list", "get", "create", "update", "delete", "sync", "unsync", "migrate", "grant", "replace", "rollback"}
	for resource, permissions := range allowed {
		for _, action := range actions {
			t.Run(resource+"/"+action, func(t *testing.T) {
				want := false
				for _, permission := range permissions {
					if permission == action {
						want = true
					}
				}
				if got := knownPermission(resource, action); got != want {
					t.Errorf("knownPermission(%q, %q) = %v, want %v", resource, action, got, want)
				}
			})
		}
	}
}

package authorization

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const validPolicy = `{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"alice","role":"viewer"}]}`

func TestParseAssignment(t *testing.T) {
	t.Run("binding error index", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			binding Binding
		}{
			{"invalid fields", Binding{Kind: rbacv1.UserKind, Name: "alice"}},
			{"invalid name format", Binding{Kind: rbacv1.UserKind, Name: " alice", Role: RoleViewer}},
			{"restricted prefix", Binding{Kind: rbacv1.UserKind, Name: "system:alice", Role: RoleViewer}},
			{"duplicate", Binding{Kind: rbacv1.UserKind, Name: "zulu", Role: RoleViewer}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				policy := Policy{Version: 1, Namespace: "team-a", Bindings: []Binding{
					{Kind: rbacv1.UserKind, Name: "zulu", Role: RoleViewer},
					tc.binding,
				}}
				payload, err := json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				assignment, err := ParseAssignment(assignmentConfigMap(string(payload)), "capp-system", nil, []string{"system:"})
				if err == nil || !strings.HasPrefix(err.Error(), "bindings[1]: ") {
					t.Fatalf("ParseAssignment() error = %v, want bindings[1] index", err)
				}
				if assignment != nil {
					t.Fatal("ParseAssignment() returned an assignment for an invalid policy")
				}
			})
		}
	})
	t.Run("name restrictions", func(t *testing.T) {
		for _, tc := range []struct {
			name, kind, identity string
			prefixes             []string
			wantErr              bool
		}{
			{"authenticated group", rbacv1.GroupKind, "system:authenticated", []string{"system:"}, true},
			{"service account", rbacv1.UserKind, "system:serviceaccount:team-a:worker", []string{"system:"}, true},
			{"custom prefix", rbacv1.UserKind, "external:alice", []string{"system:", "external:"}, true},
			{"ordinary group", rbacv1.GroupKind, "developers", []string{"system:"}, false},
			{"case sensitive", rbacv1.GroupKind, "System:authenticated", []string{"system:"}, false},
			{"prefix only", rbacv1.UserKind, "alice-system:admin", []string{"system:"}, false},
			{"leading whitespace", rbacv1.UserKind, " alice", nil, true},
			{"trailing whitespace", rbacv1.GroupKind, "developers ", nil, true},
			{"Unicode surrounding whitespace", rbacv1.UserKind, "\u00a0alice", nil, true},
			{"embedded control", rbacv1.UserKind, "ali\nce", nil, true},
			{"Unicode control", rbacv1.GroupKind, "dev\u0085ops", nil, true},
			{"internal space", rbacv1.GroupKind, "platform operators", nil, false},
			{"format character unchanged", rbacv1.UserKind, "ali\u200bce", nil, false},
			{"explicit unrestricted", rbacv1.GroupKind, "system:authenticated", []string{}, false},
			{"no library default", rbacv1.GroupKind, "system:authenticated", nil, false},
			{"empty prefix", rbacv1.UserKind, "alice", []string{""}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				policy := Policy{Version: 1, Namespace: "team-a", Bindings: []Binding{{Kind: tc.kind, Name: tc.identity, Role: RoleEditor}}}
				payload, err := json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				_, err = ParseAssignment(assignmentConfigMap(string(payload)), "capp-system", nil, tc.prefixes)
				if (err != nil) != tc.wantErr {
					t.Fatalf("ParseAssignment() error = %v, want error %v", err, tc.wantErr)
				}
				if err != nil && strings.Contains(err.Error(), tc.identity) {
					t.Fatalf("error exposes identity: %v", err)
				}
			})
		}
	})
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"valid", validPolicy, false},
		{"empty grants", `{"version":1,"namespace":"team-a","bindings":[]}`, false},
		{"missing version", `{"namespace":"team-a","bindings":[]}`, true},
		{"unsupported version", strings.Replace(validPolicy, `"version":1`, `"version":2`, 1), true},
		{"fractional version", strings.Replace(validPolicy, `"version":1`, `"version":1.0`, 1), true},
		{"missing namespace", `{"version":1,"bindings":[]}`, true},
		{"missing bindings", `{"version":1,"namespace":"team-a"}`, true},
		{"null bindings", `{"version":1,"namespace":"team-a","bindings":null}`, true},
		{"invalid namespace", strings.Replace(validPolicy, "team-a", "Team.A", 1), true},
		{"wrong target", strings.Replace(validPolicy, "team-a", "team-b", 1), true},
		{"duplicate field", strings.Replace(validPolicy, `"version":1`, `"version":1,"version":1`, 1), true},
		{"escaped duplicate field", strings.Replace(validPolicy, `"version":1`, `"version":1,"\u0076ersion":1`, 1), true},
		{"duplicate binding field", strings.Replace(validPolicy, `"name":"alice"`, `"name":"alice","name":"bob"`, 1), true},
		{"unknown field", strings.Replace(validPolicy, `"version":1`, `"secret-value":1,"version":1`, 1), true},
		{"wrong field case", strings.Replace(validPolicy, `"version"`, `"Version"`, 1), true},
		{"unknown binding field", strings.Replace(validPolicy, `"kind":"User"`, `"extra":true,"kind":"User"`, 1), true},
		{"null binding", `{"version":1,"namespace":"team-a","bindings":[null]}`, true},
		{"missing kind", strings.Replace(validPolicy, `"kind":"User",`, "", 1), true},
		{"missing name", strings.Replace(validPolicy, `"name":"alice",`, "", 1), true},
		{"missing role", strings.Replace(validPolicy, `,"role":"viewer"`, "", 1), true},
		{"null name", strings.Replace(validPolicy, `"alice"`, "null", 1), true},
		{"blank name", strings.Replace(validPolicy, `"alice"`, `" \t"`, 1), true},
		{"unsupported kind", strings.Replace(validPolicy, `"User"`, `"ServiceAccount"`, 1), true},
		{"unsupported role", strings.Replace(validPolicy, `"viewer"`, `"admin"`, 1), true},
		{"empty role", strings.Replace(validPolicy, `"viewer"`, `""`, 1), true},
		{"null role", strings.Replace(validPolicy, `"viewer"`, `null`, 1), true},
		{"wrong role case", strings.Replace(validPolicy, `"viewer"`, `"Viewer"`, 1), true},
		{"duplicate binding", strings.Replace(validPolicy, `}]`, `},{"kind":"User","name":"alice","role":"viewer"}]`, 1), true},
		{"trailing document", validPolicy + `{}`, true},
		{"malformed", `{`, true},
		{"null document", `null`, true},
		{"at payload limit", validPolicy + strings.Repeat(" ", maxPolicyBytes-len(validPolicy)), false},
		{"over payload limit", validPolicy + strings.Repeat(" ", maxPolicyBytes-len(validPolicy)+1), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := assignmentConfigMap(tt.payload)
			before := cm.DeepCopy()
			got, err := ParseAssignment(cm, "capp-system", nil, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseAssignment() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr && got != nil {
				t.Fatal("invalid assignment returned a usable result")
			}
			if !tt.wantErr && got.Namespace != "team-a" {
				t.Errorf("namespace = %q, want team-a", got.Namespace)
			}
			if diff := cmp.Diff(before, cm); diff != "" {
				t.Errorf("input mutated (-want +got): %s", diff)
			}
			if err != nil && (strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "secret-value")) {
				t.Errorf("error exposes policy data: %v", err)
			}
		})
	}
	t.Run("metadata", func(t *testing.T) {
		tests := []struct {
			name   string
			change func(*corev1.ConfigMap)
		}{
			{"wrong location", func(cm *corev1.ConfigMap) { cm.Namespace = "team-a" }},
			{"wrong name", func(cm *corev1.ConfigMap) { cm.Name = "other-assignment" }},
			{"unsupported label", func(cm *corev1.ConfigMap) { cm.Labels[assignmentLabel] = "unsupported" }},
			{"missing label", func(cm *corev1.ConfigMap) { cm.Labels = nil }},
			{"missing data", func(cm *corev1.ConfigMap) { cm.Data = nil }},
			{"extra data", func(cm *corev1.ConfigMap) { cm.Data["extra"] = "" }},
			{"binary data", func(cm *corev1.ConfigMap) { cm.BinaryData = map[string][]byte{"extra": {}} }},
			{"policy namespace target", func(cm *corev1.ConfigMap) {
				cm.Name = "capp-assignment-capp-system"
				cm.Data[policyDataKey] = strings.Replace(validPolicy, "team-a", "capp-system", 1)
			}},
			{"protected target", func(cm *corev1.ConfigMap) {
				cm.Name = "capp-assignment-kube-system"
				cm.Data[policyDataKey] = strings.Replace(validPolicy, "team-a", "kube-system", 1)
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				cm := assignmentConfigMap(validPolicy)
				tt.change(cm)
				before := cm.DeepCopy()
				got, err := ParseAssignment(cm, "capp-system", []string{"kube-system"}, nil)
				if err == nil || got != nil {
					t.Fatalf("ParseAssignment() = %v, %v; want nil and error", got, err)
				}
				if diff := cmp.Diff(before, cm); diff != "" {
					t.Errorf("input mutated (-want +got): %s", diff)
				}
			})
		}
		if _, err := ParseAssignment(nil, "capp-system", nil, nil); err == nil {
			t.Error("nil ConfigMap accepted")
		}
		if _, err := ParseAssignment(assignmentConfigMap(validPolicy), "", nil, nil); err == nil {
			t.Error("empty policy namespace accepted")
		}
	})
}

func TestAssignmentAllows(t *testing.T) {
	cm := assignmentConfigMap(`{"version":1,"namespace":"team-a","bindings":[
		{"kind":"User","name":"alice","role":"viewer"},
		{"kind":"User","name":"bob","role":"editor"},
		{"kind":"Group","name":"readers","role":"viewer"},
		{"kind":"Group","name":"developers","role":"editor"},
		{"kind":"Group","name":"alice","role":"editor"},
		{"kind":"User","name":"alice smith","role":"viewer"},
		{"kind":"User","name":"carol","role":"viewer"},
		{"kind":"User","name":"carol","role":"editor"}
	]}`)
	assignment, err := ParseAssignment(cm, "capp-system", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		user         string
		groups       []string
		allowedRoles []string
		want         bool
	}{
		{name: "viewer read", user: "alice", allowedRoles: []string{RoleViewer, RoleEditor}, want: true},
		{name: "viewer cannot mutate", user: "alice", allowedRoles: []string{RoleEditor}},
		{name: "editor read", user: "bob", allowedRoles: []string{RoleViewer, RoleEditor}, want: true},
		{name: "editor mutation", user: "bob", allowedRoles: []string{RoleEditor}, want: true},
		{name: "no implicit inheritance", user: "bob", allowedRoles: []string{RoleViewer}},
		{name: "viewer group read", user: "other", groups: []string{"readers"}, allowedRoles: []string{RoleViewer, RoleEditor}, want: true},
		{name: "viewer group cannot mutate", user: "other", groups: []string{"readers"}, allowedRoles: []string{RoleEditor}},
		{name: "editor group", user: "other", groups: []string{"unassigned", "developers"}, allowedRoles: []string{RoleEditor}, want: true},
		{name: "empty username denies matching group", groups: []string{"developers"}, allowedRoles: []string{RoleEditor}},
		{name: "user and group grants combine", user: "alice", groups: []string{"developers"}, allowedRoles: []string{RoleEditor}, want: true},
		{name: "group grants combine", user: "other", groups: []string{"readers", "developers"}, allowedRoles: []string{RoleEditor}, want: true},
		{name: "same identity retains editor", user: "carol", allowedRoles: []string{RoleEditor}, want: true},
		{name: "same identity retains viewer", user: "carol", allowedRoles: []string{RoleViewer}, want: true},
		{name: "same name different kind", user: "other", groups: []string{"alice"}, allowedRoles: []string{RoleEditor}, want: true},
		{name: "group is not username", user: "developers", allowedRoles: []string{RoleEditor}},
		{name: "user is not group", user: "other", groups: []string{"bob"}, allowedRoles: []string{RoleEditor}},
		{name: "exact user case", user: "Alice", allowedRoles: []string{RoleViewer, RoleEditor}},
		{name: "exact group case", user: "other", groups: []string{"Developers"}, allowedRoles: []string{RoleEditor}},
		{name: "internal space is significant", user: "alicesmith", allowedRoles: []string{RoleViewer}},
		{name: "preserved internal space", user: "alice smith", allowedRoles: []string{RoleViewer}, want: true},
		{name: "unassigned", user: "other", allowedRoles: []string{RoleViewer, RoleEditor}},
		{name: "nil allowed roles", user: "alice", groups: []string{"developers"}},
		{name: "empty allowed roles", user: "alice", allowedRoles: []string{}},
		{name: "unknown allowed role", user: "alice", allowedRoles: []string{"admin"}},
		{name: "blank allowed role", user: "alice", allowedRoles: []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assignment.Allows(tt.user, tt.groups, tt.allowedRoles); got != tt.want {
				t.Errorf("Allows(%q, %v, %v) = %v, want %v", tt.user, tt.groups, tt.allowedRoles, got, tt.want)
			}
		})
	}
	empty, err := ParseAssignment(assignmentConfigMap(`{"version":1,"namespace":"team-a","bindings":[]}`), "capp-system", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]*Assignment{"nil assignment": nil, "zero assignment": {}, "empty bindings": empty} {
		t.Run(name, func(t *testing.T) {
			if a.Allows("alice", []string{"developers"}, []string{RoleViewer, RoleEditor}) {
				t.Error("Allows(alice, [developers], [viewer editor]) = true, want false")
			}
		})
	}
}

func assignmentConfigMap(payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "capp-assignment-team-a", Namespace: "capp-system", Labels: map[string]string{assignmentLabel: backendPolicy}}, Data: map[string]string{policyDataKey: payload}}
}

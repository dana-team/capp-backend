package authorization

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const validPolicy = `{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"alice","role":"capp-user"}]}`

func TestParseAssignment(t *testing.T) {
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
		{"missing role", strings.Replace(validPolicy, `,"role":"capp-user"`, "", 1), true},
		{"null name", strings.Replace(validPolicy, `"alice"`, "null", 1), true},
		{"blank name", strings.Replace(validPolicy, `"alice"`, `" \t"`, 1), true},
		{"unsupported kind", strings.Replace(validPolicy, `"User"`, `"ServiceAccount"`, 1), true},
		{"unsupported role", strings.Replace(validPolicy, `"capp-user"`, `"admin"`, 1), true},
		{"duplicate binding", strings.Replace(validPolicy, `}]`, `},{"kind":"User","name":"alice","role":"capp-user"}]`, 1), true},
		{"trailing document", validPolicy + `{}`, true},
		{"malformed", `{`, true},
		{"null document", `null`, true},
		{"archived schema", `{"version":1,"namespace":"team-a","roles":{"admin":["alice"]}}`, true},
		{"at payload limit", validPolicy + strings.Repeat(" ", maxPolicyBytes-len(validPolicy)), false},
		{"over payload limit", validPolicy + strings.Repeat(" ", maxPolicyBytes-len(validPolicy)+1), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := assignmentConfigMap(tt.payload)
			before := cm.DeepCopy()
			got, err := ParseAssignment(cm, "capp-system", nil)
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
}

func TestParseAssignmentMetadata(t *testing.T) {
	tests := []struct {
		name   string
		change func(*corev1.ConfigMap)
	}{
		{"wrong location", func(cm *corev1.ConfigMap) { cm.Namespace = "team-a" }},
		{"missing label", func(cm *corev1.ConfigMap) { cm.Labels = nil }},
		{"old label", func(cm *corev1.ConfigMap) { cm.Labels[assignmentLabel] = "application" }},
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
			got, err := ParseAssignment(cm, "capp-system", []string{"kube-system"})
			if err == nil || got != nil {
				t.Fatalf("ParseAssignment() = %v, %v; want nil and error", got, err)
			}
			if diff := cmp.Diff(before, cm); diff != "" {
				t.Errorf("input mutated (-want +got): %s", diff)
			}
		})
	}
	if _, err := ParseAssignment(nil, "capp-system", nil); err == nil {
		t.Error("nil ConfigMap accepted")
	}
	if _, err := ParseAssignment(assignmentConfigMap(validPolicy), "", nil); err == nil {
		t.Error("empty policy namespace accepted")
	}
}

func TestAssignmentMatches(t *testing.T) {
	cm := assignmentConfigMap(`{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"alice","role":"capp-user"},{"kind":"Group","name":"developers","role":"capp-user"},{"kind":"Group","name":"alice","role":"capp-user"},{"kind":"User","name":" padded ","role":"capp-user"}]}`)
	assignment, err := ParseAssignment(cm, "capp-system", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, user string
		groups     []string
		want       bool
	}{
		{"user", "alice", nil, true},
		{"group", "bob", []string{"other", "developers"}, true},
		{"same name different kind", "bob", []string{"alice"}, true},
		{"group is not username", "developers", nil, false},
		{"exact user case", "Alice", nil, false},
		{"exact group case", "bob", []string{"Developers"}, false},
		{"no trimming", "padded", nil, false},
		{"preserved whitespace", " padded ", nil, true},
		{"unassigned", "bob", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assignment.Matches(tt.user, tt.groups); got != tt.want {
				t.Errorf("Matches(%q, %v) = %v, want %v", tt.user, tt.groups, got, tt.want)
			}
		})
	}
	userOnly, err := ParseAssignment(assignmentConfigMap(validPolicy), "capp-system", nil)
	if err != nil {
		t.Fatal(err)
	}
	if userOnly.Matches("bob", []string{"alice"}) {
		t.Error("user binding matched a group")
	}
	empty, err := ParseAssignment(assignmentConfigMap(`{"version":1,"namespace":"team-a","bindings":[]}`), "capp-system", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []*Assignment{nil, {}, empty} {
		if a.Matches("alice", []string{"developers"}) {
			t.Error("empty assignment granted access")
		}
	}
}

func assignmentConfigMap(payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "capp-assignment-team-a", Namespace: "capp-system", Labels: map[string]string{assignmentLabel: saRBAC}}, Data: map[string]string{policyDataKey: payload}}
}

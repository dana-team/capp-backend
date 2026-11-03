package authorization

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestManagedState(t *testing.T) {
	policy, ns, rb := readyObjects("team-a")
	rb.Subjects = append(rb.Subjects, rbacv1.Subject{
		Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "another-user",
	})
	if got := managedState(policy, ns, rb); got != ManagedObjectDrift {
		t.Errorf("managedState() with an additional subject = %q, want %q", got, ManagedObjectDrift)
	}
}

func TestManagedStateBindingOwnership(t *testing.T) {
	cases := []struct {
		name   string
		change func(*rbacv1.RoleBinding)
	}{
		{
			name: "missing management label",
			change: func(rb *rbacv1.RoleBinding) {
				delete(rb.Labels, managedByLabel)
			},
		},
		{
			name: "different policy instance",
			change: func(rb *rbacv1.RoleBinding) {
				rb.Annotations[policyUIDAnnotation] = "previous-policy-uid"
			},
		},
		{
			name: "different namespace instance",
			change: func(rb *rbacv1.RoleBinding) {
				rb.Annotations[namespaceUIDAnnotation] = "previous-namespace-uid"
			},
		},
		{
			name: "different target namespace",
			change: func(rb *rbacv1.RoleBinding) {
				rb.Annotations[targetNamespaceAnnotation] = "team-b"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, ns, rb := readyObjects("team-a")
			tc.change(rb)
			if got := managedState(policy, ns, rb); got != OwnershipConflict {
				t.Errorf("managedState() with %s = %q, want %q", tc.name, got, OwnershipConflict)
			}
		})
	}
}

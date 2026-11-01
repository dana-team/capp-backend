package authorization

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func BenchmarkAssignmentAllows(b *testing.B) {
	for _, size := range []int{16, 1024} {
		cm := benchmarkPolicy(size)
		assignment, err := ParseAssignment(cm, "capp-system", nil, nil)
		if err != nil {
			b.Fatal(err)
		}
		for _, groupCount := range []int{1, 32} {
			groups := make([]string, groupCount)
			for i := range groups {
				groups[i] = fmt.Sprintf("unassigned-%04d", i)
			}
			matchingGroups := slices.Clone(groups)
			matchingGroups[len(matchingGroups)-1] = fmt.Sprintf("identity-%04d", size-2)
			cases := []struct {
				name   string
				user   string
				groups []string
				want   bool
			}{
				{name: "early user", user: "identity-0001", groups: groups, want: true},
				{name: "late group", user: "unassigned", groups: matchingGroups, want: true},
				{name: "denied", user: "unassigned", groups: groups},
			}
			for _, c := range cases {
				b.Run(fmt.Sprintf("bindings=%d/groups=%d/%s", size, groupCount, c.name), func(b *testing.B) {
					allowedRoles := []string{RoleEditor}
					for b.Loop() {
						if got := assignment.Allows(c.user, c.groups, allowedRoles); got != c.want {
							b.Fatalf("Allows(%q, %v, %v) = %v, want %v", c.user, c.groups, allowedRoles, got, c.want)
						}
					}
				})
			}
		}
	}
}

func BenchmarkParseAssignment(b *testing.B) {
	for _, size := range []int{16, 1024} {
		b.Run(fmt.Sprintf("bindings=%d", size), func(b *testing.B) {
			cm := benchmarkPolicy(size)
			for b.Loop() {
				if _, err := ParseAssignment(cm, "capp-system", nil, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkPolicy(size int) *corev1.ConfigMap {
	bindings := make([]string, size)
	for i := range bindings {
		kind := rbacv1.UserKind
		if i%2 == 0 {
			kind = rbacv1.GroupKind
		}
		bindings[i] = fmt.Sprintf(`{"kind":%q,"name":"identity-%04d","role":%q}`, kind, i, RoleEditor)
	}
	return assignmentConfigMap(fmt.Sprintf(`{"version":1,"namespace":"team-a","bindings":[%s]}`, strings.Join(bindings, ",")))
}

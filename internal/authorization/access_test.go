package authorization

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestStoreAuthorize(t *testing.T) {
	for _, tt := range []struct {
		name      string
		namespace string
		username  string
		groups    []string
		roles     []string
		change    func(*Store, *corev1.ConfigMap, *corev1.Namespace)
		want      error
	}{
		{name: "viewer read", namespace: "team-a", username: "alice", roles: []string{RoleViewer, RoleEditor}},
		{name: "viewer mutation", namespace: "team-a", username: "alice", roles: []string{RoleEditor}, want: ErrDenied},
		{name: "editor mutation", namespace: "team-a", username: "alice", roles: []string{RoleEditor}, change: func(_ *Store, cm *corev1.ConfigMap, _ *corev1.Namespace) {
			cm.Data["policy.json"] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"User","name":"alice","role":"editor"}]}`
		}},
		{name: "additive group editor", namespace: "team-a", username: "alice", groups: []string{"developers"}, roles: []string{RoleEditor}},
		{name: "empty namespace", username: "alice", roles: []string{RoleViewer}, want: ErrDenied},
		{name: "invalid namespace", namespace: "team/a", username: "alice", roles: []string{RoleViewer}, want: ErrDenied},
		{name: "empty identity", namespace: "team-a", groups: []string{"developers"}, roles: []string{RoleEditor}, want: ErrDenied},
		{name: "wrong namespace", namespace: "team-b", username: "alice", roles: []string{RoleViewer}, want: ErrDenied},
		{name: "no role rule", namespace: "team-a", username: "alice", want: ErrDenied},
		{name: "protected precedes synchronization", namespace: "capp-system", username: "alice", roles: []string{RoleViewer}, change: func(s *Store, _ *corev1.ConfigMap, _ *corev1.Namespace) { s.synced = false }, want: ErrProtectedNamespace},
		{name: "initial observation incomplete", namespace: "team-a", username: "alice", roles: []string{RoleViewer}, change: func(s *Store, _ *corev1.ConfigMap, _ *corev1.Namespace) { s.synced = false }, want: ErrUnavailable},
		{name: "assigned awaiting association", namespace: "team-a", username: "alice", roles: []string{RoleViewer}, change: func(_ *Store, cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Annotations = nil }, want: ErrUnavailable},
		{name: "unassigned awaiting association", namespace: "team-a", username: "bob", roles: []string{RoleViewer}, change: func(_ *Store, cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Annotations = nil }, want: ErrDenied},
		{name: "unmanaged", namespace: "team-a", username: "alice", roles: []string{RoleViewer}, change: func(_ *Store, _ *corev1.ConfigMap, ns *corev1.Namespace) { ns.Labels = nil }, want: ErrDenied},
		{name: "recreated namespace", namespace: "team-a", username: "alice", roles: []string{RoleViewer}, change: func(_ *Store, _ *corev1.ConfigMap, ns *corev1.Namespace) { ns.UID = "replacement" }, want: ErrDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cm, ns := readyObjects("team-a")
			cm.ResourceVersion = "policy-version"
			s := staticStore(t, cm, ns)
			if tt.change != nil {
				tt.change(s, cm, ns)
				s.changed(policies, cm)
			}
			state, err := s.Authorize(tt.namespace, tt.username, tt.groups, tt.roles)
			require.ErrorIs(t, err, tt.want)
			if tt.want == nil {
				require.Equal(t, cm.UID, state.PolicyUID)
				require.Equal(t, cm.ResourceVersion, state.PolicyVersion)
				require.Equal(t, ns.UID, state.Namespace.UID)
			}
		})
	}
}

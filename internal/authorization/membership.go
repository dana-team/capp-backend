package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// ErrInvalidMembership indicates invalid users or an oversized membership payload.
	ErrInvalidMembership = errors.New("invalid namespace membership")
	// ErrMembershipConflict indicates that membership changed since the snapshot was read.
	ErrMembershipConflict = errors.New("membership changed; reload and retry")
)

// ReplaceUsers replaces direct capp-user members, preserving operator bindings.
// Nil leaves membership unchanged; an empty slice clears it.
// Call after authorization and setup, using current namespace metadata.
// Writes use the snapshot's store client and policy namespace.
// Returns ErrInvalidMembership or ErrMembershipConflict as applicable.
// Returns the sorted persisted direct users, or nil when users is nil (no write).
// Successful writes and conflicts invalidate the local store without waiting for
// reload. Writes are not retried, and captured records are never modified.
func (s *Snapshot) ReplaceUsers(ctx context.Context, namespace *corev1.Namespace, users []string) ([]string, error) {
	return s.writeUsers(ctx, namespace, users, false)
}

// AddUsers adds direct capp-user members without duplicates; nil leaves membership
// unchanged. It has the same caller prerequisites and error contract as ReplaceUsers.
func (s *Snapshot) AddUsers(ctx context.Context, namespace *corev1.Namespace, users []string) ([]string, error) {
	return s.writeUsers(ctx, namespace, users, true)
}

func (s *Snapshot) writeUsers(ctx context.Context, namespace *corev1.Namespace, users []string, add bool) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if users == nil {
		return nil, nil
	}
	record, persistedUsers, err := s.prepareMembership(namespace, users, add)
	if err != nil {
		return nil, err
	}
	exists := record.ResourceVersion != ""
	if exists {
		// Patch only policy.json and the snapshot version, preserving other data and metadata.
		base := record.DeepCopy()
		base.Data = nil
		err = s.store.target.Patch(ctx, record, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	} else {
		err = s.store.target.Create(ctx, record)
	}
	if k8serrors.IsConflict(err) || k8serrors.IsAlreadyExists(err) || (exists && k8serrors.IsNotFound(err)) {
		s.store.Invalidate()
		return nil, ErrMembershipConflict
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Kubernetes diagnostics can include submitted policy values.
		return nil, errors.New("cannot persist namespace membership")
	}
	s.store.Invalidate()
	return persistedUsers, nil
}

// prepareMembership builds a validated record without mutating the snapshot.
// Existing records retain their snapshot version; new records have no version.
func (s *Snapshot) prepareMembership(namespace *corev1.Namespace, users []string, add bool) (*corev1.ConfigMap, []string, error) {
	if s == nil || s.store == nil || s.generation == nil || s.generation.records == nil {
		return nil, nil, errors.New("authorization snapshot is unavailable")
	}
	policyNamespace := s.store.namespace
	if namespace == nil || namespace.Name == "" || namespace.UID == "" || policyNamespace == "" {
		return nil, nil, errors.New("membership requires current namespace metadata and a policy namespace")
	}
	for _, user := range users {
		// JSON encoding replaces invalid UTF-8, which would change the identity granted access.
		if !utf8.ValidString(user) {
			return nil, nil, ErrInvalidMembership
		}
	}
	membership, exists := s.generation.records.memberships[namespace.UID]
	if exists && membership.Namespace.Name != namespace.Name {
		return nil, nil, errors.New("membership identity does not match current namespace")
	}
	if !exists {
		membership = membershipPolicy{
			Version: 1, Namespace: namespaceIdentity{Name: namespace.Name, UID: namespace.UID},
			Bindings: []binding{},
		}
	}
	if add {
		membership.Users = append(slices.Clone(membership.Users), users...)
		slices.Sort(membership.Users)
		membership.Users = slices.Compact(membership.Users)
	} else {
		membership.Users = slices.Clone(users)
		slices.Sort(membership.Users)
	}
	if membership.Users == nil {
		membership.Users = []string{}
	}
	if len(membership.Users) > 0 {
		if _, ok := s.generation.records.cluster.Roles[memberRole]; !ok {
			return nil, nil, errors.New("users reference undefined capp-user role")
		}
	}
	payload, err := json.Marshal(membership)
	if err != nil {
		return nil, nil, ErrInvalidMembership
	}
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: membershipNamePrefix + string(namespace.UID), Namespace: policyNamespace,
		Labels: map[string]string{policyLabelKey: policyLabelValue},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1.SchemeGroupVersion.String(), Kind: namespaceKind, Name: namespace.Name, UID: namespace.UID,
		}},
	}}
	if exists {
		record.ResourceVersion = s.generation.identities[record.Name].version
		if record.ResourceVersion == "" {
			return nil, nil, errors.New("membership snapshot is missing its resource version")
		}
	}
	record.Data = map[string]string{policyDataKey: string(payload)}
	if err := validateRecord(record, policyNamespace); err != nil {
		return nil, nil, ErrInvalidMembership
	}
	if _, err := parseMembershipPolicy(record); err != nil {
		return nil, nil, ErrInvalidMembership
	}
	return record, membership.Users, nil
}

// Package authorization validates application policies and evaluates access for verified identities.
package authorization

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// policyModel is bundled with the evaluator so authorization semantics are versioned together.
//
//go:embed model.conf
var policyModel string

// Snapshot holds one target's policy and one verified caller's identity for a request.
// Checks must be used sequentially within that request; Casbin maintains internal
// evaluation state. Construct a separate snapshot for each caller and target.
type Snapshot struct {
	enforcer         *casbin.Enforcer
	subjects         []string
	records          *policyRecords
	resourceVersions map[string]string
	listVersion      string
}

// Load reads and validates all labeled policy records in one unpaginated LIST.
// The caller must supply a direct, uncached service-account client for the target
// and a credential produced by authentication. Policy errors never produce a usable snapshot.
func Load(ctx context.Context, target client.Client, cfg config.ClusterAuthorizationConfig, credential auth.ClusterCredential) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(credential.ImpersonateUser) == "" {
		return nil, errors.New("authorization requires a verified username")
	}
	if cfg.PolicyNamespace == "" {
		return nil, errors.New("authorization requires a policy namespace")
	}
	subjects := make([]string, 0, 1+len(credential.ImpersonateGroups))
	subjects = append(subjects, policyIdentifier(rbacv1.UserKind, credential.ImpersonateUser))
	for _, group := range credential.ImpersonateGroups {
		if strings.TrimSpace(group) == "" {
			return nil, errors.New("authorization requires nonblank verified groups")
		}
		subjects = append(subjects, policyIdentifier(rbacv1.GroupKind, group))
	}

	var list corev1.ConfigMapList
	if err := target.List(ctx, &list, client.InNamespace(cfg.PolicyNamespace), client.MatchingLabels{policyLabelKey: policyLabelValue}); err != nil {
		return nil, fmt.Errorf("read authorization policy: %w", err)
	}
	if list.Continue != "" {
		return nil, errors.New("authorization policy list is incomplete")
	}
	records, err := parsePolicyRecords(list.Items, cfg.PolicyNamespace)
	if err != nil {
		return nil, err
	}
	enforcer, err := compilePolicy(records)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	versions := make(map[string]string, len(list.Items))
	for i := range list.Items {
		versions[list.Items[i].Name] = list.Items[i].ResourceVersion
	}
	return &Snapshot{
		enforcer: enforcer, subjects: subjects, records: records,
		resourceVersions: versions, listVersion: list.ResourceVersion,
	}, nil
}

// ResourceVersion returns the policy LIST version for audit correlation.
func (s *Snapshot) ResourceVersion() string {
	return s.listVersion
}

// CheckCluster checks a permission that applies to cluster-level operations.
func (s *Snapshot) CheckCluster(resource, action string) (bool, error) {
	return s.check(clusterScope, resource, action)
}

// CheckAllNamespaces checks an explicit allNamespaces grant, such as delegation
// before namespace creation. A grant on one existing namespace cannot satisfy it.
func (s *Snapshot) CheckAllNamespaces(resource, action string) (bool, error) {
	return s.check(allNamespacesScope, resource, action)
}

// CheckNamespace checks access to a current namespace instance. The caller must
// pass metadata read from this target during the request, never request-body identity.
// Aggregate operations can reuse one namespace LIST across all checks.
func (s *Snapshot) CheckNamespace(namespace *corev1.Namespace, resource, action string) (bool, error) {
	if namespace == nil || namespace.Name == "" || namespace.UID == "" {
		return false, errors.New("authorization requires current namespace metadata")
	}
	if s == nil || s.records == nil {
		return false, errors.New("authorization snapshot is unavailable")
	}
	if membership, ok := s.records.memberships[namespace.UID]; ok && membership.Namespace.Name != namespace.Name {
		return false, errors.New("membership identity does not match current namespace")
	}
	return s.check("ns/"+string(namespace.UID), resource, action)
}

func (s *Snapshot) check(domain, resource, action string) (bool, error) {
	if s == nil || s.enforcer == nil {
		return false, errors.New("authorization snapshot is unavailable")
	}
	if !knownPermission(resource, action) {
		return false, nil
	}
	allowed := false
	for _, subject := range s.subjects {
		granted, err := s.enforcer.Enforce(subject, domain, resource, action)
		if err != nil {
			// Casbin diagnostics may contain policy or identity values.
			return false, errors.New("authorization policy evaluation failed")
		}
		// Evaluate every subject so an error cannot be hidden by an earlier grant.
		allowed = allowed || granted
	}
	return allowed, nil
}

func compilePolicy(records *policyRecords) (*casbin.Enforcer, error) {
	policy, err := model.NewModelFromString(policyModel)
	if err != nil {
		return nil, errors.New("invalid authorization model")
	}
	enforcer, err := casbin.NewEnforcer(policy)
	if err != nil {
		return nil, errors.New("cannot initialize authorization evaluator")
	}
	enforcer.EnableAutoSave(false)

	var permissions, bindings [][]string
	compiledRoles := make(map[[2]string]bool)
	addBinding := func(grant binding, domain string) {
		role := policyIdentifier("Role", grant.Role)
		bindings = append(bindings, []string{policyIdentifier(grant.Kind, grant.Name), role, domain})
		key := [2]string{role, domain}
		if compiledRoles[key] {
			return
		}
		compiledRoles[key] = true
		for _, permission := range records.cluster.Roles[grant.Role] {
			for _, action := range permission.Actions {
				permissions = append(permissions, []string{role, domain, permission.Resource, action})
			}
		}
	}
	for _, grant := range records.cluster.Bindings {
		addBinding(grant.binding, grant.Scope)
	}
	for uid, membership := range records.memberships {
		domain := "ns/" + string(uid)
		for _, user := range membership.Users {
			addBinding(binding{Kind: rbacv1.UserKind, Name: user, Role: memberRole}, domain)
		}
		for _, grant := range membership.Bindings {
			addBinding(grant, domain)
		}
	}
	if len(permissions) > 0 {
		if _, err := enforcer.AddPolicies(permissions); err != nil {
			return nil, errors.New("cannot compile authorization permissions")
		}
	}
	if len(bindings) > 0 {
		if _, err := enforcer.AddGroupingPolicies(bindings); err != nil {
			return nil, errors.New("cannot compile authorization bindings")
		}
	}
	return enforcer, nil
}

// policyIdentifier separates identity kinds and prevents Casbin's internal
// delimiter-based policy keys from conflating names containing delimiters.
func policyIdentifier(kind, name string) string {
	return kind + "/" + base64.RawURLEncoding.EncodeToString([]byte(name))
}

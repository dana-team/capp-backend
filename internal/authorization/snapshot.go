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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// policyModel is bundled with the evaluator so authorization semantics are versioned together.
//
//go:embed model.conf
var policyModel string

// Snapshot holds one target's policy and one verified caller's identity for a request.
// Checks are safe to use concurrently. Caller subjects stay request-local while
// immutable policy evaluators can be shared by snapshots for the same target.
type Snapshot struct {
	store            *Store
	generation       *policyGeneration
	namespaces       *namespaceGeneration
	namespaceVersion string
	evaluators       *policyEvaluators
	subjects         []string
	records          *policyRecords
	resourceVersions map[string]string
	listVersion      string
}

// policyEvaluators is compiled in full before use and never mutated afterward.
// Namespace evaluators include global grants but exclude other namespaces' rows.
type policyEvaluators struct {
	cluster       *casbin.SyncedEnforcer
	allNamespaces *casbin.SyncedEnforcer
	namespaces    map[types.UID]*casbin.SyncedEnforcer
}

// Load reads and validates all labeled policy records in one unpaginated LIST.
// The caller must supply a direct, uncached service-account client for the target
// and a credential produced by authentication. Policy errors never produce a usable snapshot.
func Load(ctx context.Context, target client.Client, cfg config.ClusterAuthorizationConfig, credential auth.ClusterCredential) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	subjects, err := verifiedSubjects(credential)
	if err != nil {
		return nil, err
	}
	if cfg.PolicyNamespace == "" {
		return nil, errors.New("authorization requires a policy namespace")
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
	evaluators, err := compilePolicyEvaluators(records)
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
		evaluators: evaluators, subjects: subjects, records: records,
		resourceVersions: versions, listVersion: list.ResourceVersion,
	}, nil
}

// ResourceVersion returns the policy LIST version for audit correlation.
func (s *Snapshot) ResourceVersion() string {
	return s.listVersion
}

// Namespace returns a defensive copy of captured metadata, or Kubernetes NotFound
// for a missing name in a usable collection. It never falls back to a live read.
func (s *Snapshot) Namespace(name string) (*corev1.Namespace, error) {
	if err := s.validateNamespaces(); err != nil {
		return nil, err
	}
	namespace := s.namespaces.byName[name].DeepCopy()
	if err := s.validateNamespaces(); err != nil {
		return nil, err
	}
	if namespace == nil {
		return nil, k8serrors.NewNotFound(corev1.Resource("namespaces"), name)
	}
	return namespace, nil
}

// Namespaces returns defensive copies of the complete captured collection sorted
// by name. Callers must apply their configured access and discovery filters.
func (s *Snapshot) Namespaces() ([]corev1.Namespace, error) {
	if err := s.validateNamespaces(); err != nil {
		return nil, err
	}
	namespaces := make([]corev1.Namespace, 0, len(s.namespaces.names))
	for _, name := range s.namespaces.names {
		namespaces = append(namespaces, *s.namespaces.byName[name].DeepCopy())
	}
	if err := s.validateNamespaces(); err != nil {
		return nil, err
	}
	return namespaces, nil
}

// NamespaceResourceVersion returns the captured namespace LIST version for audit.
func (s *Snapshot) NamespaceResourceVersion() (string, error) {
	if err := s.validateNamespaces(); err != nil {
		return "", err
	}
	return s.namespaceVersion, nil
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

// CheckNamespace checks access to a namespace instance. Shared snapshots verify
// the supplied identity against their captured namespace collection. Direct-load
// callers must supply metadata read from the target, never request-body identity.
func (s *Snapshot) CheckNamespace(namespace *corev1.Namespace, resource, action string) (bool, error) {
	if namespace == nil || namespace.Name == "" || namespace.UID == "" {
		return false, errors.New("authorization requires current namespace metadata")
	}
	if s == nil || s.records == nil {
		return false, errors.New("authorization snapshot is unavailable")
	}
	if s.store != nil {
		cached, err := s.Namespace(namespace.Name)
		if err != nil {
			return false, err
		}
		if cached.UID != namespace.UID {
			return false, ErrPolicyUnavailable
		}
		namespace = cached
	}
	if membership, ok := s.records.memberships[namespace.UID]; ok && membership.Namespace.Name != namespace.Name {
		return false, errors.New("membership identity does not match current namespace")
	}
	return s.check("ns/"+string(namespace.UID), resource, action)
}

func (s *Snapshot) validateNamespaces() error {
	if s == nil || s.store == nil {
		return ErrPolicyUnavailable
	}
	return s.store.validateNamespaces(s.namespaces)
}

func (s *Snapshot) check(domain, resource, action string) (bool, error) {
	if s == nil || s.evaluators == nil {
		return false, errors.New("authorization snapshot is unavailable")
	}
	if s.store != nil {
		if err := s.store.validate(s.generation, s.namespaces, domain != clusterScope && domain != allNamespacesScope); err != nil {
			return false, err
		}
	}
	allowed, err := s.evaluate(domain, resource, action)
	if s.store != nil {
		if err := s.store.validate(s.generation, s.namespaces, domain != clusterScope && domain != allNamespacesScope); err != nil {
			return false, err
		}
	}
	return allowed, err
}

func (s *Snapshot) evaluate(domain, resource, action string) (bool, error) {
	if !knownPermission(resource, action) {
		return false, nil
	}
	enforcer := s.evaluators.allNamespaces
	switch domain {
	case clusterScope:
		enforcer = s.evaluators.cluster
	case allNamespacesScope:
		// Only explicit global grants can satisfy this scope.
	default:
		if scoped, ok := s.evaluators.namespaces[types.UID(strings.TrimPrefix(domain, "ns/"))]; ok {
			enforcer = scoped
		}
	}
	allowed := false
	for _, subject := range s.subjects {
		granted, err := enforcer.Enforce(subject, domain, resource, action)
		if err != nil {
			// Casbin diagnostics may contain policy or identity values.
			return false, errors.New("authorization policy evaluation failed")
		}
		// Evaluate every subject so an error cannot be hidden by an earlier grant.
		allowed = allowed || granted
	}
	return allowed, nil
}

func verifiedSubjects(credential auth.ClusterCredential) ([]string, error) {
	if strings.TrimSpace(credential.ImpersonateUser) == "" {
		return nil, errors.New("authorization requires a verified username")
	}
	subjects := make([]string, 0, 1+len(credential.ImpersonateGroups))
	subjects = append(subjects, policyIdentifier(rbacv1.UserKind, credential.ImpersonateUser))
	for _, group := range credential.ImpersonateGroups {
		if strings.TrimSpace(group) == "" {
			return nil, errors.New("authorization requires nonblank verified groups")
		}
		subjects = append(subjects, policyIdentifier(rbacv1.GroupKind, group))
	}
	return subjects, nil
}

// compilePolicyEvaluators requires globally validated records: filtering first
// would let invalid records outside the requested namespace escape validation.
func compilePolicyEvaluators(records *policyRecords) (*policyEvaluators, error) {
	cluster := policyRecords{cluster: clusterPolicy{Roles: records.cluster.Roles}}
	global := policyRecords{cluster: clusterPolicy{Roles: records.cluster.Roles}}
	for _, grant := range records.cluster.Bindings {
		if grant.Scope == clusterScope {
			cluster.cluster.Bindings = append(cluster.cluster.Bindings, grant)
		} else {
			global.cluster.Bindings = append(global.cluster.Bindings, grant)
		}
	}
	clusterEnforcer, err := compilePolicy(&cluster)
	if err != nil {
		return nil, err
	}
	globalEnforcer, err := compilePolicy(&global)
	if err != nil {
		return nil, err
	}
	evaluators := &policyEvaluators{
		cluster: clusterEnforcer, allNamespaces: globalEnforcer,
		namespaces: make(map[types.UID]*casbin.SyncedEnforcer, len(records.memberships)),
	}
	for uid, membership := range records.memberships {
		scoped := policyRecords{
			cluster:     global.cluster,
			memberships: map[types.UID]membershipPolicy{uid: membership},
		}
		enforcer, err := compilePolicy(&scoped)
		if err != nil {
			return nil, err
		}
		evaluators.namespaces[uid] = enforcer
	}
	return evaluators, nil
}

func compilePolicy(records *policyRecords) (*casbin.SyncedEnforcer, error) {
	policy, err := model.NewModelFromString(policyModel)
	if err != nil {
		return nil, errors.New("invalid authorization model")
	}
	enforcer, err := casbin.NewSyncedEnforcer(policy)
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

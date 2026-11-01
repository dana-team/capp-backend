package authorization

import (
	"errors"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/json"
)

const (
	policyLabelKey       = consts.CappAPIGroup + "/authorization"
	policyLabelValue     = "true"
	namespaceKind        = "Namespace"
	policyDataKey        = "policy.json"
	clusterPolicyName    = "capp-policy"
	membershipNamePrefix = "capp-members-"
	memberRole           = "capp-user"
	maxPolicyRecordBytes = 512 * 1024
	clusterScope         = "cluster"
	allNamespacesScope   = "allNamespaces"
	namespaceScopePrefix = "ns/"
)

// Actions recognized by the application permission validator. Valid actions vary by resource.
const (
	ActionGet     = "get"
	ActionList    = "list"
	ActionCreate  = "create"
	ActionUpdate  = "update"
	ActionDelete  = "delete"
	ActionSync    = "sync"
	ActionUnsync  = "unsync"
	ActionMigrate = "migrate"
	ActionGrant   = "grant"
	ActionReplace = "replace"
)

// Resources recognized by the application permission validator.
const (
	ResourceClusters      = "clusters"
	ResourceNamespaces    = "namespaces"
	ResourceCapps         = consts.CappResource
	ResourceSecrets       = "secrets"
	ResourceConfigMaps    = "configmaps"
	ResourceCappRevisions = "capprevisions"
	ResourceBenchmarks    = "benchmarks"
	ResourceCappUserRole  = "roles/" + memberRole
)

type permission struct {
	Resource string   `json:"resource"`
	Actions  []string `json:"actions"`
}

type binding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type clusterBinding struct {
	binding
	Scope string `json:"scope"`
}

type clusterPolicy struct {
	Version  int                     `json:"version"`
	Roles    map[string][]permission `json:"roles"`
	Bindings []clusterBinding        `json:"bindings"`
}

type namespaceIdentity struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

type membershipPolicy struct {
	Version   int               `json:"version"`
	Namespace namespaceIdentity `json:"namespace"`
	Users     []string          `json:"users"`
	Bindings  []binding         `json:"bindings"`
}

type policyRecords struct {
	cluster     clusterPolicy
	memberships map[types.UID]membershipPolicy
}

// parsePolicyRecords validates a complete set of labeled records, independent of LIST order.
// Snapshot checks match memberships against captured namespace identities.
func parsePolicyRecords(records []corev1.ConfigMap, policyNamespace string) (*policyRecords, error) {
	result := &policyRecords{memberships: make(map[types.UID]membershipPolicy)}
	seen := make(map[string]bool, len(records))
	for i := range records {
		record := &records[i]
		if seen[record.Name] {
			return nil, errors.New("duplicate policy record")
		}
		seen[record.Name] = true
		if err := validateRecord(record, policyNamespace); err != nil {
			return nil, err
		}
		if record.Name == clusterPolicyName {
			policy, err := parseClusterPolicy(record)
			if err != nil {
				return nil, err
			}
			result.cluster = policy
			continue
		}
		membership, err := parseMembershipPolicy(record)
		if err != nil {
			return nil, err
		}
		result.memberships[membership.Namespace.UID] = membership
	}
	if !seen[clusterPolicyName] {
		return nil, errors.New("missing cluster policy")
	}
	for _, membership := range result.memberships {
		if len(membership.Users) > 0 {
			if _, ok := result.cluster.Roles[memberRole]; !ok {
				return nil, errors.New("users reference undefined capp-user role")
			}
		}
		for _, grant := range membership.Bindings {
			if err := validateBinding(grant, result.cluster.Roles); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func validateRecord(record *corev1.ConfigMap, policyNamespace string) error {
	if policyNamespace == "" || record.Namespace != policyNamespace {
		return errors.New("policy record is outside policy namespace")
	}
	if record.Labels[policyLabelKey] != policyLabelValue {
		return errors.New("missing authorization label")
	}
	if _, ok := record.Labels[consts.ManagedLabelKey]; ok {
		return errors.New("policy record must not have managed-resource label")
	}
	if len(validation.NameIsDNSSubdomain(record.Name, false)) != 0 {
		return errors.New("invalid policy record name")
	}
	payload, ok := record.Data[policyDataKey]
	if !ok {
		return errors.New("missing policy.json data")
	}
	if len(payload) > maxPolicyRecordBytes {
		return errors.New("policy record exceeds 512 KiB")
	}
	return nil
}

func parseClusterPolicy(record *corev1.ConfigMap) (clusterPolicy, error) {
	var policy clusterPolicy
	if err := decodePolicy(record.Data[policyDataKey], &policy); err != nil {
		return policy, err
	}
	if policy.Version != 1 || policy.Roles == nil || policy.Bindings == nil {
		return policy, errors.New("cluster policy requires version 1, roles and bindings")
	}
	for _, owner := range record.OwnerReferences {
		if owner.Kind == namespaceKind {
			return policy, errors.New("cluster policy must not have a namespace owner")
		}
	}
	if err := validateRoles(policy.Roles); err != nil {
		return policy, err
	}
	for _, grant := range policy.Bindings {
		if grant.Scope != clusterScope && grant.Scope != allNamespacesScope {
			return policy, errors.New("unsupported binding scope")
		}
		if err := validateBinding(grant.binding, policy.Roles); err != nil {
			return policy, err
		}
	}
	return policy, nil
}

func parseMembershipPolicy(record *corev1.ConfigMap) (membershipPolicy, error) {
	var policy membershipPolicy
	if err := decodePolicy(record.Data[policyDataKey], &policy); err != nil {
		return policy, err
	}
	if policy.Version != 1 || policy.Users == nil || policy.Bindings == nil {
		return policy, errors.New("membership requires version 1, users and bindings")
	}
	if len(validation.ValidateNamespaceName(policy.Namespace.Name, false)) != 0 || strings.TrimSpace(string(policy.Namespace.UID)) == "" {
		return policy, errors.New("invalid membership namespace identity")
	}
	if record.Name != membershipNamePrefix+string(policy.Namespace.UID) {
		return policy, errors.New("membership name does not match namespace UID")
	}
	if err := validateNamespaceOwner(record, policy.Namespace); err != nil {
		return policy, err
	}
	seen := make(map[string]bool, len(policy.Users))
	for _, user := range policy.Users {
		if strings.TrimSpace(user) == "" {
			return policy, errors.New("blank membership user")
		}
		if seen[user] {
			return policy, errors.New("duplicate membership user")
		}
		seen[user] = true
	}
	return policy, nil
}

func validateNamespaceOwner(record *corev1.ConfigMap, namespace namespaceIdentity) error {
	found := false
	for _, owner := range record.OwnerReferences {
		if owner.Kind != namespaceKind {
			continue
		}
		if found || owner.APIVersion != corev1.SchemeGroupVersion.String() || owner.Name != namespace.Name || owner.UID != namespace.UID {
			return errors.New("membership namespace owner does not match identity")
		}
		if owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion {
			return errors.New("membership must not block namespace deletion")
		}
		found = true
	}
	if !found {
		return errors.New("missing membership namespace owner")
	}
	return nil
}

func decodePolicy(payload string, target any) error {
	strictErrors, err := json.UnmarshalStrict([]byte(payload), target)
	// Decoder errors can contain policy values; keep validation failures safe to log.
	if err != nil {
		return errors.New("invalid policy JSON")
	}
	if len(strictErrors) != 0 {
		return errors.New("duplicate or unknown policy JSON fields")
	}
	return nil
}

func validateRoles(roles map[string][]permission) error {
	for name, permissions := range roles {
		if strings.TrimSpace(name) == "" || permissions == nil {
			return errors.New("role requires a name and permissions array")
		}
		for _, permission := range permissions {
			if len(permission.Actions) == 0 {
				return errors.New("permission requires actions")
			}
			for _, action := range permission.Actions {
				if !knownPermission(permission.Resource, action) {
					return errors.New("unsupported resource or action")
				}
			}
		}
	}
	return nil
}

func validateBinding(grant binding, roles map[string][]permission) error {
	if grant.Kind != rbacv1.UserKind && grant.Kind != rbacv1.GroupKind {
		return errors.New("unsupported binding kind")
	}
	if strings.TrimSpace(grant.Name) == "" || strings.TrimSpace(grant.Role) == "" {
		return errors.New("binding requires a name and role")
	}
	if _, ok := roles[grant.Role]; !ok {
		return errors.New("binding references undefined role")
	}
	return nil
}

func knownPermission(resource, action string) bool {
	for _, permission := range permissionInventory() {
		if permission.Resource == resource {
			return slices.Contains(permission.Actions, action)
		}
	}
	return false
}

// permissionInventory defines the supported resource/action pairs for validation
// and discovery. Namespace discovery excludes cluster-only operations.
func permissionInventory() []permission {
	return []permission{
		{Resource: ResourceClusters, Actions: []string{ActionGet}},
		{Resource: ResourceNamespaces, Actions: []string{ActionGet, ActionCreate, ActionUpdate, ActionDelete}},
		{Resource: ResourceCapps, Actions: []string{ActionList, ActionGet, ActionCreate, ActionUpdate, ActionDelete, ActionSync, ActionUnsync, ActionMigrate}},
		{Resource: ResourceSecrets, Actions: []string{ActionList, ActionGet, ActionCreate, ActionUpdate, ActionDelete}},
		{Resource: ResourceConfigMaps, Actions: []string{ActionList, ActionGet, ActionCreate, ActionUpdate, ActionDelete}},
		{Resource: ResourceCappRevisions, Actions: []string{ActionList, ActionGet}},
		{Resource: ResourceBenchmarks, Actions: []string{ActionGet, ActionCreate}},
		{Resource: ResourceCappUserRole, Actions: []string{ActionGrant, ActionReplace}},
	}
}

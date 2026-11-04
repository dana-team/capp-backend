package middleware

import (
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/internal/authorization"
)

// PolicyCategory determines how an authenticated API operation is prepared.
type PolicyCategory string

// Policy categories separate public endpoints and the different target scopes.
const (
	PolicyPublic         PolicyCategory = "public"
	PolicyDiscovery      PolicyCategory = "discovery"
	PolicyNamespace      PolicyCategory = "namespace"
	PolicyAggregate      PolicyCategory = "aggregate"
	PolicyMigration      PolicyCategory = "migration"
	PolicyDenied         PolicyCategory = "denied"
	PolicyAdministration PolicyCategory = "administration"
)

// PolicyRule declares the roles accepted by one API operation.
type PolicyRule struct {
	Category     PolicyCategory
	AllowedRoles []string
}

const (
	namespaceCollectionPath = "/api/v1/clusters/:cluster/namespaces"
	namespacePolicyPath     = namespaceCollectionPath + "/:namespace"
)

type policyRoute struct{ method, path string }

// PolicyRoutes is the fixed API-role map, keyed by method and registered template.
// It is immutable after construction. Lookup returns a detached rule.
type PolicyRoutes struct {
	rules map[policyRoute]PolicyRule
}

// NewPolicyRoutes builds the API-role map. An empty metricsPath disables its
// public rule; a configured path must be literal, canonical, and must not replace
// another rule.
func NewPolicyRoutes(metricsPath string) (*PolicyRoutes, error) {
	const clusterPath = "/api/v1/clusters/:cluster"
	read := PolicyRule{Category: PolicyNamespace, AllowedRoles: []string{authorization.RoleViewer, authorization.RoleEditor}}
	write := PolicyRule{Category: PolicyNamespace, AllowedRoles: []string{authorization.RoleEditor}}
	routes := &PolicyRoutes{rules: make(map[policyRoute]PolicyRule)}
	type declaration struct {
		method string
		path   string
		rule   PolicyRule
	}
	var declarations []declaration
	for _, path := range []string{"/healthz", "/readyz", "/docs", "/ui/scalar.js", "/openapi.yaml", "/api/v1/sizes", "/api/v1/auth/mode", "/api/v1/auth/openshift/authorize"} {
		declarations = append(declarations, declaration{method: http.MethodGet, path: path, rule: PolicyRule{Category: PolicyPublic}})
	}
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/openshift/callback"} {
		declarations = append(declarations, declaration{method: http.MethodPost, path: path, rule: PolicyRule{Category: PolicyPublic}})
	}
	for _, path := range []string{"/api/v1/clusters", clusterPath, namespaceCollectionPath} {
		declarations = append(declarations, declaration{method: http.MethodGet, path: path, rule: PolicyRule{Category: PolicyDiscovery, AllowedRoles: read.AllowedRoles}})
	}
	declarations = append(declarations,
		declaration{method: http.MethodGet, path: namespacePolicyPath, rule: read},
		declaration{method: http.MethodPost, path: namespaceCollectionPath, rule: PolicyRule{Category: PolicyAdministration}},
	)
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		declarations = append(declarations, declaration{method: method, path: namespacePolicyPath, rule: PolicyRule{Category: PolicyAdministration}})
	}
	for _, resource := range []string{"capps", "configmaps", "secrets"} {
		path := namespacePolicyPath + "/" + resource
		declarations = append(declarations,
			declaration{method: http.MethodGet, path: clusterPath + "/" + resource, rule: PolicyRule{Category: PolicyAggregate, AllowedRoles: read.AllowedRoles}},
			declaration{method: http.MethodGet, path: path, rule: read},
			declaration{method: http.MethodGet, path: path + "/:name", rule: read},
			declaration{method: http.MethodPost, path: path, rule: write},
			declaration{method: http.MethodPut, path: path + "/:name", rule: write},
			declaration{method: http.MethodDelete, path: path + "/:name", rule: write},
		)
		if resource != "capps" {
			declarations = append(declarations, declaration{method: http.MethodGet, path: path + "/names", rule: read})
		}
	}
	cappPath := namespacePolicyPath + "/capps/:name"
	declarations = append(declarations,
		declaration{method: http.MethodGet, path: cappPath + "/revisions", rule: read},
		declaration{method: http.MethodGet, path: cappPath + "/revisions/:revision", rule: read},
		declaration{method: http.MethodPost, path: cappPath + "/revisions/:revision/rollback", rule: write},
		declaration{method: http.MethodPost, path: cappPath + "/sync", rule: write},
		declaration{method: http.MethodDelete, path: cappPath + "/sync", rule: write},
		declaration{method: http.MethodPost, path: cappPath + "/migrate", rule: PolicyRule{Category: PolicyMigration, AllowedRoles: write.AllowedRoles}},
		declaration{method: http.MethodGet, path: cappPath + "/benchmark", rule: PolicyRule{Category: PolicyDenied}},
		declaration{method: http.MethodPost, path: cappPath + "/benchmark", rule: PolicyRule{Category: PolicyDenied}},
	)
	for _, path := range []string{"dashboards", "query", "query_range"} {
		declarations = append(declarations, declaration{method: http.MethodGet, path: cappPath + "/metrics/" + path, rule: PolicyRule{Category: PolicyDenied}})
	}
	if metricsPath != "" {
		if !strings.HasPrefix(metricsPath, "/") || strings.ContainsAny(metricsPath, ":*?#") {
			return nil, fmt.Errorf("metrics path must be an absolute literal path")
		}
		// Gin cleans registered paths but preserves a single trailing slash.
		canonicalPath := path.Clean(metricsPath)
		if strings.HasSuffix(metricsPath, "/") && canonicalPath != "/" {
			canonicalPath += "/"
		}
		if metricsPath != canonicalPath {
			return nil, fmt.Errorf("metrics path must be canonical: use %q", canonicalPath)
		}
		declarations = append(declarations, declaration{method: http.MethodGet, path: metricsPath, rule: PolicyRule{Category: PolicyPublic}})
	}
	for _, entry := range declarations {
		key := policyRoute{method: entry.method, path: entry.path}
		if _, exists := routes.rules[key]; exists {
			return nil, fmt.Errorf("duplicate API policy rule: %s %s", entry.method, entry.path)
		}
		routes.rules[key] = entry.rule
	}
	return routes, nil
}

// Lookup returns the rule for an exact HTTP method and registered route template.
// Unknown operations return a denied rule and false.
func (r *PolicyRoutes) Lookup(method, path string) (PolicyRule, bool) {
	if r == nil {
		return PolicyRule{Category: PolicyDenied}, false
	}
	rule, ok := r.rules[policyRoute{method: method, path: path}]
	if !ok {
		return PolicyRule{Category: PolicyDenied}, false
	}
	rule.AllowedRoles = slices.Clone(rule.AllowedRoles)
	return rule, true
}

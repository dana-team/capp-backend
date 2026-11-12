package policy

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/config"
)

// Category determines how an authenticated API operation is prepared.
type Category string

// Policy categories separate public endpoints and the different target scopes.
const (
	Public         Category = "public"
	Discovery      Category = "discovery"
	Namespace      Category = "namespace"
	Aggregate      Category = "aggregate"
	Migration      Category = "migration"
	Denied         Category = "denied"
	Administration Category = "administration"
)

// Rule declares the roles accepted by one API operation.
type Rule struct {
	Category     Category
	AllowedRoles []string
}

const (
	clusterPath             = "/api/v1/clusters/:cluster"
	namespaceCollectionPath = clusterPath + "/namespaces"
	namespacePath           = namespaceCollectionPath + "/:namespace"
)

type policyRoute struct{ method, path string }

// Routes is the fixed API-role map, keyed by method and registered template.
// It is immutable after construction. Lookup returns a detached rule.
type Routes struct {
	rules map[policyRoute]Rule
}

// NewRoutes builds the API-role map. An empty metricsPath disables its
// public rule; a configured path must be literal, canonical, outside the /api
// subtree, and must not replace another rule.
func NewRoutes(metricsPath string) (*Routes, error) {
	read := Rule{Category: Namespace, AllowedRoles: []string{authorization.RoleViewer, authorization.RoleEditor}}
	write := Rule{Category: Namespace, AllowedRoles: []string{authorization.RoleEditor}}
	type declaration struct {
		method string
		path   string
		rule   Rule
	}
	var declarations []declaration
	for _, path := range []string{"/healthz", "/readyz", "/docs", "/ui/scalar.js", "/openapi.yaml", "/api/v1/sizes", "/api/v1/auth/mode", "/api/v1/auth/openshift/authorize"} {
		declarations = append(declarations, declaration{method: http.MethodGet, path: path, rule: Rule{Category: Public}})
	}
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/openshift/callback"} {
		declarations = append(declarations, declaration{method: http.MethodPost, path: path, rule: Rule{Category: Public}})
	}
	for _, path := range []string{"/api/v1/clusters", clusterPath, namespaceCollectionPath} {
		declarations = append(declarations, declaration{method: http.MethodGet, path: path, rule: Rule{Category: Discovery, AllowedRoles: read.AllowedRoles}})
	}
	declarations = append(declarations,
		declaration{method: http.MethodGet, path: namespacePath, rule: read},
		declaration{method: http.MethodPost, path: namespaceCollectionPath, rule: Rule{Category: Administration}},
	)
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		declarations = append(declarations, declaration{method: method, path: namespacePath, rule: Rule{Category: Administration}})
	}
	for _, resource := range []string{"capps", "configmaps", "secrets"} {
		path := namespacePath + "/" + resource
		declarations = append(declarations,
			declaration{method: http.MethodGet, path: clusterPath + "/" + resource, rule: Rule{Category: Aggregate, AllowedRoles: read.AllowedRoles}},
			declaration{method: http.MethodGet, path: path, rule: read},
			declaration{method: http.MethodGet, path: path + "/:name", rule: read},
			declaration{method: http.MethodPost, path: path, rule: write},
			declaration{method: http.MethodPut, path: path + "/:name", rule: write},
			declaration{method: http.MethodDelete, path: path + "/:name", rule: write},
		)
		if resource != "capps" {
			declarations = append(declarations,
				declaration{method: http.MethodGet, path: path + "/names", rule: read},
				declaration{method: http.MethodGet, path: path + "/:name/bound", rule: read},
			)
		}
	}
	cappPath := namespacePath + "/capps/:name"
	declarations = append(declarations,
		declaration{method: http.MethodGet, path: cappPath + "/revisions", rule: read},
		declaration{method: http.MethodGet, path: cappPath + "/revisions/:revision", rule: read},
		declaration{method: http.MethodPost, path: cappPath + "/revisions/:revision/rollback", rule: write},
		declaration{method: http.MethodPost, path: cappPath + "/rollout", rule: write},
		declaration{method: http.MethodDelete, path: cappPath + "/rollout/strategy", rule: write},
		declaration{method: http.MethodPost, path: cappPath + "/sync", rule: write},
		declaration{method: http.MethodDelete, path: cappPath + "/sync", rule: write},
		declaration{method: http.MethodPost, path: cappPath + "/migrate", rule: Rule{Category: Migration, AllowedRoles: write.AllowedRoles}},
		declaration{method: http.MethodGet, path: cappPath + "/benchmark", rule: Rule{Category: Denied}},
		declaration{method: http.MethodPost, path: cappPath + "/benchmark", rule: Rule{Category: Denied}},
	)
	for _, path := range []string{"dashboards", "query", "query_range"} {
		declarations = append(declarations, declaration{method: http.MethodGet, path: cappPath + "/metrics/" + path, rule: Rule{Category: Denied}})
	}
	if metricsPath != "" {
		if err := config.ValidateMetricsPath(metricsPath); err != nil {
			return nil, err
		}
		declarations = append(declarations, declaration{method: http.MethodGet, path: metricsPath, rule: Rule{Category: Public}})
	}
	routes := &Routes{rules: make(map[policyRoute]Rule)}
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
func (r *Routes) Lookup(method, path string) (Rule, bool) {
	if r == nil {
		return Rule{Category: Denied}, false
	}
	rule, ok := r.rules[policyRoute{method: method, path: path}]
	if !ok {
		return Rule{Category: Denied}, false
	}
	rule.AllowedRoles = slices.Clone(rule.AllowedRoles)
	return rule, true
}

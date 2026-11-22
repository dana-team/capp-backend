// Package namespaces holds the namespace request/response DTOs of the REST API.
//
// It imports nothing, so clients (cappctl, capp-mcp) can share these types
// without pulling in the server handler and its Kubernetes/Gin dependencies.
// The handler package internal/resources/cluster/namespaces aliases them.
package namespaces

// NamespaceListResponse is the response envelope for the list endpoint.
type NamespaceListResponse struct {
	Items     []NamespaceItem `json:"items"`
	Total     int             `json:"total"`
	CanCreate bool            `json:"canCreate"`
}

// UsedQuotaInfo holds current resource usage within a namespace's quota.
type UsedQuotaInfo struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   *int   `json:"pods,omitempty"`
}

// QuotaInfo holds the hard limits and current usage from a namespace's ResourceQuota.
type QuotaInfo struct {
	CPU    string         `json:"cpu,omitempty"`
	Memory string         `json:"memory,omitempty"`
	Pods   *int           `json:"pods,omitempty"`
	Used   *UsedQuotaInfo `json:"used,omitempty"`
}

// NamespaceItem is the simplified namespace representation returned to the frontend.
type NamespaceItem struct {
	// Name is the Kubernetes namespace name.
	Name string `json:"name"`

	// Status is the namespace phase: "Active" or "Terminating".
	Status string `json:"status"`

	// Quota holds the resource quota hard limits and current usage.
	// Nil when no ResourceQuota exists for the namespace.
	Quota *QuotaInfo `json:"quota,omitempty"`

	// Users contains direct User/editor policy assignments.
	// Nil when membership is unavailable, including an invalid policy.
	Users *[]string `json:"users,omitempty"`

	// Bindings contains the full policy assignment set when membership is available.
	Bindings              *[]Binding `json:"bindings,omitempty"`
	PolicyResourceVersion string     `json:"policyResourceVersion,omitempty"`
	PolicyState           string     `json:"policyState,omitempty"`

	// CanEdit is true when the authenticated user is a configured operator
	// allowed to modify the namespace's quota and membership.
	CanEdit bool `json:"canEdit"`
}

// ResourceQuota is the requested CPU/memory/pod limits for a namespace.
type ResourceQuota struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   int    `json:"pods,omitempty"`
}

// CreateNamespaceRequest specifies a new namespace's membership and quota.
type CreateNamespaceRequest struct {
	Bindings *[]Binding     `json:"bindings,omitempty"`
	Name     string         `json:"name" binding:"required"`
	Users    *[]string      `json:"users,omitempty"`
	Quota    *ResourceQuota `json:"quota,omitempty"`
}

// UpdateNamespaceRequest replaces the supplied namespace membership and quota.
type UpdateNamespaceRequest struct {
	Bindings              *[]Binding     `json:"bindings,omitempty"`
	PolicyResourceVersion *string        `json:"policyResourceVersion,omitempty"`
	Users                 *[]string      `json:"users,omitempty"`
	Quota                 *ResourceQuota `json:"quota,omitempty"`
}

// PatchNamespaceRequest adds namespace membership without removing existing grants.
type PatchNamespaceRequest struct {
	Bindings              *[]Binding `json:"bindings,omitempty"`
	PolicyResourceVersion *string    `json:"policyResourceVersion,omitempty"`
	Users                 *[]string  `json:"users,omitempty"`
}

// Binding assigns an application role to a named user or group.
// Names must be nonblank, without surrounding whitespace or control characters.
type Binding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Role string `json:"role"`
}

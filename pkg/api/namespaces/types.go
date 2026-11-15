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

	// Users is the list of users with access to the namespace.
	// Nil when no RoleBinding exists for the namespace.
	Users *[]string `json:"users,omitempty"`

	// CanEdit is true when the authenticated user has admin-level permission
	// to modify the namespace's quota and full user list (via PUT).
	CanEdit bool `json:"canEdit"`
}

// ResourceQuota is the requested CPU/memory/pod limits for a namespace.
type ResourceQuota struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   int    `json:"pods,omitempty"`
}

type CreateNamespaceRequest struct {
	Name  string         `json:"name" binding:"required"`
	Users *[]string      `json:"users,omitempty"`
	Quota *ResourceQuota `json:"quota,omitempty"`
}

type UpdateNamespaceRequest struct {
	Users *[]string      `json:"users,omitempty"`
	Quota *ResourceQuota `json:"quota,omitempty"`
}

type PatchNamespaceRequest struct {
	Users *[]string `json:"users,omitempty"`
}

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

	// Users contains sorted direct API-managed users in application authorization
	// mode, excluding operator bindings and expanded groups. Legacy mode uses
	// RoleBinding users and may omit the field when no binding exists.
	Users *[]string `json:"users,omitempty"`

	// CanEdit hints whether the caller can modify the quota and full user list.
	// Application authorization requires namespace update and user replacement
	// permissions. Mutations authorize independently of this hint.
	CanEdit bool `json:"canEdit"`
}

type resourceQuota struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   int    `json:"pods,omitempty"`
}

type CreateNamespaceRequest struct {
	Name  string         `json:"name" binding:"required"`
	Users *[]string      `json:"users,omitempty"`
	Quota *resourceQuota `json:"quota,omitempty"`
}

type UpdateNamespaceRequest struct {
	Users *[]string      `json:"users,omitempty"`
	Quota *resourceQuota `json:"quota,omitempty"`
}

type PatchNamespaceRequest struct {
	Users *[]string `json:"users,omitempty"`
}

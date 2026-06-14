package namespaces

// NamespaceListResponse is the response envelope for the list endpoint.
type NamespaceListResponse struct {
	Items     []NamespaceItem `json:"items"`
	CanCreate bool            `json:"canCreate"`
}

// NamespaceItem is the simplified namespace representation returned to the frontend.
type NamespaceItem struct {
	// Name is the Kubernetes namespace name.
	Name string `json:"name"`

	// Status is the namespace phase: "Active" or "Terminating".
	Status string `json:"status"`
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
	Users *[]string      `json:"users,omitempty"`
	Quota *resourceQuota `json:"quota,omitempty"`
}

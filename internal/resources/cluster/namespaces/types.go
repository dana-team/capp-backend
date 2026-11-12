package namespaces

import api "github.com/dana-team/capp-backend/pkg/api/namespaces"

// The DTOs live in pkg/api/namespaces so clients can import them without this
// package's dependencies; these aliases keep the handler code unchanged.
type (
	NamespaceListResponse  = api.NamespaceListResponse
	UsedQuotaInfo          = api.UsedQuotaInfo
	QuotaInfo              = api.QuotaInfo
	NamespaceItem          = api.NamespaceItem
	ResourceQuota          = api.ResourceQuota
	CreateNamespaceRequest = api.CreateNamespaceRequest
	UpdateNamespaceRequest = api.UpdateNamespaceRequest
	PatchNamespaceRequest  = api.PatchNamespaceRequest
	resourceQuota          = api.ResourceQuota
)

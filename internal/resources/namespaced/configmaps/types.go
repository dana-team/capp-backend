package configmaps

import api "github.com/dana-team/capp-backend/pkg/api/configmaps"

// The DTOs live in pkg/api/configmaps so clients can import them without this
// package's dependencies; these aliases keep the handler code unchanged.
type (
	ConfigMapRequest          = api.ConfigMapRequest
	ConfigMapUpdateRequest    = api.ConfigMapUpdateRequest
	ConfigMapResponse         = api.ConfigMapResponse
	ConfigMapListResponse     = api.ConfigMapListResponse
	ConfigMapNameListResponse = api.ConfigMapNameListResponse
)

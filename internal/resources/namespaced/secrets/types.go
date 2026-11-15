package secrets

import api "github.com/dana-team/capp-backend/pkg/api/secrets"

// The DTOs live in pkg/api/secrets so clients can import them without this
// package's dependencies; these aliases keep the handler code unchanged.
type (
	SecretRequest          = api.SecretRequest
	SecretUpdateRequest    = api.SecretUpdateRequest
	SecretResponse         = api.SecretResponse
	SecretListResponse     = api.SecretListResponse
	SecretNameListResponse = api.SecretNameListResponse
)

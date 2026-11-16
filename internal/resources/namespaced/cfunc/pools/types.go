// Package pools implements the cfunc pool resource handler.
//
// A pool is a set of warm pods of one language runtime that cfunc functions
// run in. It is stored as a fission.io/v1/Environment custom resource, and
// the handler exposes full CRUD for it:
//
//	GET    /api/v1/clusters/:cluster/cfunc/pools                               (all namespaces)
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name
//	POST   /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools
//	PUT    /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name
//	DELETE /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name
//
// A client never names an image: a request carries a flavor and a size, and
// the backend configuration (resources.cfunc) holds the images, build
// settings and resources behind them. The handler translates between the
// frontend-facing DTOs defined in this file and the Fission Environment type
// via convert.go. All K8s API interaction happens through the per-request
// scoped client injected by the cluster middleware.
package pools

import api "github.com/dana-team/capp-backend/pkg/api/cfunc"

// The DTOs live in pkg/api/cfunc so clients can import them without this
// package's dependencies; these aliases keep the handler code unchanged.
type (
	KeySelector        = api.KeySelector
	EnvVarSource       = api.EnvVarSource
	EnvVar             = api.EnvVar
	ResourceQuantities = api.ResourceQuantities
	ResourceSpec       = api.ResourceSpec
	Advanced           = api.Advanced
	PoolRequest        = api.PoolRequest
	PoolResponse       = api.PoolResponse
	PoolListResponse   = api.PoolListResponse
	PoolStatusResponse = api.PoolStatusResponse
	Builder            = api.Builder
	Condition          = api.Condition
)

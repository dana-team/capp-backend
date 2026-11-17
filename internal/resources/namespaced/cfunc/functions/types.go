// Package functions implements the cfunc function resource handler.
//
// A function is code from a git repository that runs in a cfunc pool, plus
// the triggers that call it. Fission has no single object for that, so one
// function is a fission.io/v1 Function, the Package that holds its code, and
// one HTTPTrigger or TimeTrigger for each trigger. The handler writes them
// together and exposes them as one resource:
//
//	GET    /api/v1/clusters/:cluster/cfunc/functions                               (all namespaces)
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name
//	POST   /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions
//	PUT    /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name
//	DELETE /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name
//	POST   /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name/test
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name/logs
//
// A function always runs on Fission's pool manager, in a pool of its own
// namespace. The backend clones the repository with the credential in its
// configuration (resources.cfunc.functions.git) and stores the folder inline
// in the Package. Every http trigger path is served under
// /<namespace>/<function>, because the Fission router has one URL space for
// the whole cluster.
package functions

import api "github.com/dana-team/capp-backend/pkg/api/cfunc"

// The DTOs live in pkg/api/cfunc so clients can import them without this
// package's dependencies; these aliases keep the handler code unchanged.
type (
	SecretVolume           = api.SecretVolume
	ConfigMapVolume        = api.ConfigMapVolume
	FunctionSource         = api.FunctionSource
	FunctionTrigger        = api.FunctionTrigger
	FunctionAdvanced       = api.FunctionAdvanced
	FunctionRequest        = api.FunctionRequest
	FunctionResponse       = api.FunctionResponse
	FunctionListResponse   = api.FunctionListResponse
	FunctionBuild          = api.FunctionBuild
	FunctionStatusResponse = api.FunctionStatusResponse
	FunctionTestRequest    = api.FunctionTestRequest
	FunctionTestResponse   = api.FunctionTestResponse
	FunctionLogsResponse   = api.FunctionLogsResponse
	FunctionPodLog         = api.FunctionPodLog
)

const (
	TriggerTypeHTTP = api.TriggerTypeHTTP
	TriggerTypeCron = api.TriggerTypeCron
)

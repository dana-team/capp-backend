// Package cfunc holds the cfunc (function as a service) request/response DTOs of the REST API.
//
// It imports only the shared DTOs in pkg/api/common, so clients (cappctl,
// capp-mcp) can share these types without pulling in the server handler and
// its Kubernetes/Gin dependencies.
// The handler package internal/resources/namespaced/cfunc/pools aliases them.
package cfunc

import "github.com/dana-team/capp-backend/pkg/api/common"

// The container-level DTOs are shared with other resources; these aliases
// let callers use them through this package.
type (
	ResourceQuantities = common.ResourceQuantities
	ResourceSpec       = common.ResourceSpec
	EnvVarSource       = common.EnvVarSource
	KeySelector        = common.KeySelector
	EnvVar             = common.EnvVar
)

// Advanced overrides values that normally come from the flavor and the size.
type Advanced struct {
	// BuildImage replaces the flavor's build image. It is a full image
	// reference. Rejected unless the backend enables custom builds.
	BuildImage string `json:"buildImage,omitempty"`

	// BuildCommand replaces the flavor's build command. With a build image
	// and no command anywhere, the backend uses "build". Rejected unless the
	// backend enables custom builds.
	BuildCommand string `json:"buildCommand,omitempty"`

	// Resources lays over the size's resources: a value left out here comes
	// from the size. No value may exceed the largest configured size.
	Resources *ResourceSpec `json:"resources,omitempty"`
}

// PoolRequest is the request body accepted by POST (create) and PUT (update).
// Required fields are validated by Gin's binding:"required" tags.
type PoolRequest struct {
	// Name is the Kubernetes resource name. Required for create.
	// For update it is taken from the URL parameter.
	Name string `json:"name" binding:"required"`

	// Flavor names the language runtime, one of GET /api/v1/cfunc/flavors.
	Flavor string `json:"flavor" binding:"required"`

	// Size names the pod size, one of GET /api/v1/cfunc/sizes.
	// Optional; defaults to the configured default size.
	Size string `json:"size,omitempty"`

	// PoolSize is the number of warm pods kept ready. Optional; defaults to 3.
	// It must not exceed the backend's resources.cfunc.maxPoolSize.
	PoolSize *int `json:"poolSize,omitempty"`

	// Env is the list of environment variables set on every warm pod, so
	// every function that runs in the pool sees them. Optional.
	Env []EnvVar `json:"env,omitempty"`

	// Advanced overrides the flavor's build settings and the size's resources. Optional.
	Advanced *Advanced `json:"advanced,omitempty"`
}

// Builder is the build image and command a pool runs with.
type Builder struct {
	Image   string `json:"image"`
	Command string `json:"command,omitempty"`
}

// Condition is one status condition reported by Fission.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// PoolStatusResponse is the state Fission reports for the pool's Environment.
type PoolStatusResponse struct {
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions"`
}

// PoolResponse is the response body for single-pool endpoints.
type PoolResponse struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	CreatedAt       string            `json:"createdAt,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`

	// Flavor is the configured flavor whose runtime image the pool
	// uses. Empty when no configured flavor has that image.
	Flavor string `json:"flavor"`

	// Size is the configured size whose resources the pool uses.
	// Empty when no configured size has those resources.
	Size string `json:"size"`

	PoolSize  int           `json:"poolSize"`
	Resources *ResourceSpec `json:"resources,omitempty"`
	Image     string        `json:"image"`
	Builder   *Builder      `json:"builder,omitempty"`
	Env       []EnvVar      `json:"env,omitempty"`

	// Advanced holds what the pool has beyond its flavor and size: custom
	// resources, and a build image or command that is not the flavor's. It
	// is set only then, so that a client can send the response back in a
	// PUT without losing them.
	Advanced *Advanced `json:"advanced,omitempty"`

	Version                       int    `json:"version"`
	TerminationGracePeriodSeconds int64  `json:"terminationGracePeriodSeconds"`
	AllowedFunctionsPerContainer  string `json:"allowedFunctionsPerContainer,omitempty"`
	AllowAccessToExternalNetwork  bool   `json:"allowAccessToExternalNetwork,omitempty"`

	Status PoolStatusResponse `json:"status"`
}

// PoolListResponse is the response body for list endpoints.
type PoolListResponse struct {
	Items []PoolResponse `json:"items"`
	Total int            `json:"total"`
}

// FlavorListResponse is the response body of GET /api/v1/cfunc/flavors.
type FlavorListResponse struct {
	Items []string `json:"items"`
	Total int      `json:"total"`
}

// SizeListResponse is the response body of GET /api/v1/cfunc/sizes.
type SizeListResponse struct {
	Items []string `json:"items"`
	Total int      `json:"total"`
}

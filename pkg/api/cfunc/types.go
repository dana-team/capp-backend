// Package cfunc holds the cfunc (function as a service) request/response DTOs of the REST API.
//
// It imports only the shared DTOs in pkg/api/common, so clients (cappctl,
// capp-mcp) can share these types without pulling in the server handler and
// its Kubernetes/Gin dependencies.
// The handler packages under internal/resources/namespaced/cfunc alias them.
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
	SecretVolume       = common.SecretVolume
	ConfigMapVolume    = common.ConfigMapVolume
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

// Trigger types of a function.
const (
	TriggerTypeHTTP = "http"
	TriggerTypeCron = "cron"
)

// FunctionSource names the code of a function in a git repository.
type FunctionSource struct {
	// URL is the https URL of the repository. It must be under the git base
	// URL of the backend, which holds the credential.
	URL string `json:"url"`

	// Ref is a branch, a tag or a full commit hash. Optional; defaults to
	// the default branch of the repository.
	Ref string `json:"ref,omitempty"`

	// Path is the folder, or the one file, inside the repository that holds
	// the function. Optional; defaults to the repository root.
	Path string `json:"path,omitempty"`

	// Build says whether Fission builds the code before it runs. Optional;
	// defaults to true for a folder in a pool that has a build image, and to
	// false for one file, which is loaded as it is. A compiled language
	// needs true also for one file. A response always carries the value.
	Build *bool `json:"build,omitempty"`

	// Commit is the hash of the commit that is deployed. Response only.
	Commit string `json:"commit,omitempty"`
}

// FunctionTrigger is one way a function is called. Type selects which of the
// other request fields apply.
type FunctionTrigger struct {
	// Name identifies the trigger inside its function.
	Name string `json:"name"`

	// Type is "http" or "cron".
	Type string `json:"type"`

	// Path is the URL path of an http trigger below the prefix
	// /<namespace>/<function> that the backend adds. Optional; empty serves
	// the prefix itself.
	Path string `json:"path,omitempty"`

	// Methods lists the HTTP methods of an http trigger. Optional; defaults
	// to GET.
	Methods []string `json:"methods,omitempty"`

	// Schedule is the cron line of a cron trigger: five fields, six with
	// seconds first, or a descriptor such as "@hourly" or "@every 5m".
	Schedule string `json:"schedule,omitempty"`

	// FullPath is the path the router serves for an http trigger. Response only.
	FullPath string `json:"fullPath,omitempty"`

	// URL is FullPath on the cluster's Fission router, when the backend
	// knows the router URL. Response only.
	URL string `json:"url,omitempty"`

	// Status is the reason of the trigger's Ready condition, for example
	// RouteAdmitted, RouteConflict or CronRegistered. Response only.
	Status string `json:"status,omitempty"`

	// Message explains Status when the trigger is not ready. Response only.
	Message string `json:"message,omitempty"`
}

// FunctionAdvanced holds the execution settings most functions leave alone.
type FunctionAdvanced struct {
	// Concurrency is the highest number of pods that serve the function at
	// one time. It must not exceed the backend's maximum.
	Concurrency *int `json:"concurrency,omitempty"`

	// RequestsPerPod is how many requests one pod serves at the same time.
	// Defaults to 1.
	RequestsPerPod *int `json:"requestsPerPod,omitempty"`

	// IdleTimeoutSeconds is how long a pod stays with the function after its
	// last request. Defaults to 120.
	IdleTimeoutSeconds *int `json:"idleTimeoutSeconds,omitempty"`

	// OnceOnly makes a pod serve exactly one request.
	OnceOnly bool `json:"onceOnly,omitempty"`

	// RetainPods is how many pods are kept with the function after the idle
	// time. Defaults to 0.
	RetainPods *int `json:"retainPods,omitempty"`

	// SpecializationTimeoutSeconds is how long Fission waits for a pod to
	// load the code. Defaults to 120.
	SpecializationTimeoutSeconds *int `json:"specializationTimeoutSeconds,omitempty"`

	// BuildCommand replaces the pool's build command for this function.
	// Rejected unless the backend enables custom builds.
	BuildCommand string `json:"buildCommand,omitempty"`
}

// FunctionRequest is the request body accepted by POST (create) and PUT (update).
type FunctionRequest struct {
	// Name is the Kubernetes resource name. Required for create.
	// For update it is taken from the URL parameter.
	Name string `json:"name" binding:"required"`

	// Pool names the cfunc pool, in the same namespace, that runs the function.
	Pool string `json:"pool" binding:"required"`

	// Source names the code in a git repository.
	Source FunctionSource `json:"source"`

	// Entrypoint is the function inside the code that the runtime calls. Its
	// form depends on the flavor of the pool. Optional.
	Entrypoint string `json:"entrypoint,omitempty"`

	// TimeoutSeconds is the longest time one request may take. Optional;
	// defaults to 60.
	TimeoutSeconds *int `json:"timeoutSeconds,omitempty"`

	// SecretVolumes lists Secrets of the namespace the function reads as
	// files. Fission fixes the path, so name and mountPath are optional and,
	// when sent, must equal what a response shows.
	SecretVolumes []SecretVolume `json:"secretVolumes,omitempty"`

	// ConfigMapVolumes lists ConfigMaps of the namespace the function reads
	// as files, under the same rules as SecretVolumes.
	ConfigMapVolumes []ConfigMapVolume `json:"configMapVolumes,omitempty"`

	// Triggers lists the ways the function is called. Full replace on update.
	Triggers []FunctionTrigger `json:"triggers,omitempty"`

	// Advanced overrides the execution defaults. Optional.
	Advanced *FunctionAdvanced `json:"advanced,omitempty"`
}

// FunctionBuild is the build state of the function's code.
type FunctionBuild struct {
	// Status is pending, running, succeeded, failed, or none when the code
	// needs no build. Empty until Fission has seen the package.
	Status string `json:"status"`

	// Log is the output of the build.
	Log string `json:"log,omitempty"`
}

// FunctionStatusResponse is the state Fission reports for the Function.
type FunctionStatusResponse struct {
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions"`
}

// FunctionResponse is the response body for single-function endpoints.
type FunctionResponse struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	CreatedAt       string `json:"createdAt,omitempty"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`

	Pool             string            `json:"pool"`
	Source           FunctionSource    `json:"source"`
	Entrypoint       string            `json:"entrypoint,omitempty"`
	TimeoutSeconds   int               `json:"timeoutSeconds"`
	SecretVolumes    []SecretVolume    `json:"secretVolumes,omitempty"`
	ConfigMapVolumes []ConfigMapVolume `json:"configMapVolumes,omitempty"`
	Triggers         []FunctionTrigger `json:"triggers"`

	// Advanced is set only when a value differs from its default, so that a
	// client can send the response back in a PUT without losing it.
	Advanced *FunctionAdvanced `json:"advanced,omitempty"`

	Build  FunctionBuild          `json:"build"`
	Status FunctionStatusResponse `json:"status"`
}

// FunctionListResponse is the response body for list endpoints.
type FunctionListResponse struct {
	Items []FunctionResponse `json:"items"`
	Total int                `json:"total"`
}

// FunctionTestRequest is the request body of the function test route. It
// names one http trigger of the function and the request to send through it.
type FunctionTestRequest struct {
	// Trigger names an http trigger of the function. Optional when the
	// function has exactly one.
	Trigger string `json:"trigger,omitempty"`

	// Method is the HTTP method. Optional; defaults to the first method of
	// the trigger.
	Method string `json:"method,omitempty"`

	// Headers are sent with the request.
	Headers map[string]string `json:"headers,omitempty"`

	// Body is sent as the request body.
	Body string `json:"body,omitempty"`
}

// FunctionTestResponse is what the function answered.
type FunctionTestResponse struct {
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body"`
	// Truncated is true when the body was longer than the backend returns.
	Truncated  bool  `json:"truncated,omitempty"`
	DurationMs int64 `json:"durationMs"`
}

// FunctionLogsResponse is the response body of the function logs route.
type FunctionLogsResponse struct {
	Pods []FunctionPodLog `json:"pods"`
}

// FunctionPodLog is the recent log of one pod that serves the function.
type FunctionPodLog struct {
	Pod   string   `json:"pod"`
	Lines []string `json:"lines"`
}

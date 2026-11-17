// Package capps holds the Capp request/response DTOs of the REST API.
//
// It imports only the shared DTOs in pkg/api/common, so clients (cappctl,
// capp-mcp) can share these types without pulling in the server handler and
// its Kubernetes/Gin dependencies.
// The handler package internal/resources/namespaced/capps aliases them.
package capps

import "github.com/dana-team/capp-backend/pkg/api/common"

// ── Request / Response DTOs ───────────────────────────────────────────────────
// These types are the public API contract between the backend and the frontend.
// They are intentionally simpler than the raw Kubernetes Capp spec so the
// frontend does not need to understand K8s conventions.

type CappSize string

const CappSizeSmall CappSize = "small"
const CappSizeMedium CappSize = "medium"
const CappSizeLarge CappSize = "large"

// The container-level DTOs are shared with other resources; these aliases
// keep the names this package has always exported.
type (
	ResourceQuantities = common.ResourceQuantities
	ResourceSpec       = common.ResourceSpec
	EnvVarSource       = common.EnvVarSource
	KeySelector        = common.KeySelector
	EnvVar             = common.EnvVar
	SecretVolume       = common.SecretVolume
	ConfigMapVolume    = common.ConfigMapVolume
)

// VolumeMount maps a volume into a container at a path.
type VolumeMount struct {
	Name      string `json:"name"      binding:"required"`
	MountPath string `json:"mountPath" binding:"required"`
}

// RouteSpec configures the external HTTP route for a Capp.
type RouteSpec struct {
	// Hostname is a custom DNS name for the Capp. Optional.
	Hostname string `json:"hostname,omitempty"`

	// TLSEnabled determines whether TLS is enabled for the route.
	TLSEnabled bool `json:"tlsEnabled,omitempty"`

	// RouteTimeoutSeconds is the maximum request duration. Optional.
	RouteTimeoutSeconds *int64 `json:"routeTimeoutSeconds,omitempty"`

	// RolloutStrategy configures a staged rollout of new revisions. Optional.
	// Unlike other fields, omitting it on update keeps the current strategy,
	// because removing a strategy mid-rollout moves live traffic. Removal has
	// its own endpoint.
	RolloutStrategy *RolloutStrategy `json:"rolloutStrategy,omitempty"`
}

// RolloutStage is a single traffic stage of a staged rollout. The operator's
// webhook enforces ordering and which stages must set a duration.
type RolloutStage struct {
	// Percent is the candidate revision's share of traffic. A pointer because
	// 0 is valid (blue-green) while a missing value is not.
	Percent *int64 `json:"percent" binding:"required,min=0,max=100"`

	// Duration is the minimum time to hold the stage, e.g. "5m". Must be unset
	// on the last stage.
	Duration string `json:"duration,omitempty"`
}

// RolloutStrategy defines a staged rollout of a new Capp revision.
type RolloutStrategy struct {
	Stages []RolloutStage `json:"stages" binding:"required,min=1,dive"`

	// AutoRollback returns traffic to the stable revision when the candidate fails.
	AutoRollback bool `json:"autoRollback,omitempty"`

	// ProgressDeadline is how long the candidate may take to first become
	// ready, e.g. "10m". The operator defaults it to 10m.
	ProgressDeadline string `json:"progressDeadline,omitempty"`
}

// LogSpec configures log shipping for a Capp.
type LogSpec struct {
	// Type is one of "elastic" or "elastic-datastream".
	Type           string `json:"type"`
	Host           string `json:"host"`
	Target         string `json:"target"`
	User           string `json:"user"`
	PasswordSecret string `json:"passwordSecret"`
	PasswordKey    string `json:"passwordKey"`
}

// NFSVolume describes one NFS volume to be mounted into the Capp containers.
type NFSVolume struct {
	Name     string `json:"name"     binding:"required"`
	Server   string `json:"server"   binding:"required"`
	Path     string `json:"path"     binding:"required"`
	Capacity string `json:"capacity" binding:"required"` // e.g. "10Gi"
}

// ScaleSpec configures autoscaling for a Capp.
type ScaleSpec struct {
	// Metric defines which metric type is watched by the Autoscaler.
	// One of: concurrency, cpu, memory, rps. Default: concurrency.
	Metric string `json:"metric,omitempty"`

	// MinReplicas sets the minimum replica count. Optional; nil means unset.
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas sets the maximum replica count. Optional; nil means unset.
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// ScaleDelaySeconds is the delay before the Autoscaler scales down to zero.
	// Optional; nil means unset.
	ScaleDelaySeconds *int32 `json:"scaleDelaySeconds,omitempty"`
}

// CappRequest is the request body accepted by POST (create) and PUT (update).
// Required fields are validated by Gin's binding:"required" tags.
type CappRequest struct {
	// Name is the Kubernetes resource name. Required for create.
	// For update it is taken from the URL parameter.
	Name string `json:"name" binding:"required"`

	// ScaleSpec configures autoscaling. Optional.
	ScaleSpec ScaleSpec `json:"scaleSpec,omitempty"`

	// State is enabled or disabled. Default: enabled.
	State string `json:"state,omitempty"`

	// Image is the container image reference. Required.
	Image string `json:"image" binding:"required"`

	// ContainerName is the optional container name.
	ContainerName string `json:"containerName,omitempty"`

	// CappSize is a high-level sizing abstraction that maps to specific resource requests/limits.
	// Mutually exclusive with CustomResources.
	Size CappSize `json:"size,omitempty"`

	// CustomResources allows setting explicit CPU/memory requests and limits
	// when the predefined t-shirt sizes do not fit.
	// Mutually exclusive with Size.
	CustomResources *ResourceSpec `json:"customResources,omitempty"`

	// Env is the list of environment variables.
	Env []EnvVar `json:"env,omitempty"`

	// VolumeMounts maps volumes into the container.
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// RouteSpec configures external routing. Optional.
	RouteSpec *RouteSpec `json:"routeSpec,omitempty"`

	// LogSpec configures log shipping. Optional.
	LogSpec *LogSpec `json:"logSpec,omitempty"`

	// NFSVolumes lists NFS volumes to provision. Optional.
	NFSVolumes []NFSVolume `json:"nfsVolumes,omitempty"`

	// SecretVolumes lists Kubernetes Secrets to mount as volumes. Optional.
	SecretVolumes []SecretVolume `json:"secretVolumes,omitempty"`

	// ConfigMapVolumes lists Kubernetes ConfigMaps to mount as volumes. Optional.
	ConfigMapVolumes []ConfigMapVolume `json:"configMapVolumes,omitempty"`

	// ImagePullSecrets lists Secret names (type kubernetes.io/dockerconfigjson)
	// in the Capp's namespace used to pull the container image from a private
	// registry. Optional. Replaced as a whole on update; omit to clear.
	ImagePullSecrets []string `json:"imagePullSecrets,omitempty"`

	// ReloadOnBoundResourceChange restarts the Capp when a Secret or ConfigMap
	// it references is updated through this API. Stored as the
	// rcs.dana.io/rollout-on-bound-resource-change annotation. Optional;
	// omitting it on update turns it off.
	ReloadOnBoundResourceChange bool `json:"reloadOnBoundResourceChange,omitempty"`
}

// ── Response types ────────────────────────────────────────────────────────────

// ConditionResponse is a single Capp status condition.
type ConditionResponse struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// StateStatusResponse reflects the Capp's current enabled/disabled state.
type StateStatusResponse struct {
	State      string `json:"state,omitempty"`
	LastChange string `json:"lastChange,omitempty"`
}

// RevisionTrafficResponse is the traffic share routed to a single revision.
type RevisionTrafficResponse struct {
	RevisionName string `json:"revisionName"`
	Percent      int64  `json:"percent"`
	Tag          string `json:"tag"`
}

// RolloutStatusResponse is the observed state of a Capp's staged rollout.
type RolloutStatusResponse struct {
	StableRevision     string                    `json:"stableRevision,omitempty"`
	CandidateRevision  string                    `json:"candidateRevision,omitempty"`
	PinnedRevision     string                    `json:"pinnedRevision,omitempty"`
	Phase              string                    `json:"phase,omitempty"`
	StageIndex         int32                     `json:"stageIndex"`
	HeldPercent        *int64                    `json:"heldPercent,omitempty"`
	StageStartTime     string                    `json:"stageStartTime,omitempty"`
	CandidateSince     string                    `json:"candidateSince,omitempty"`
	CandidateReadyOnce bool                      `json:"candidateReadyOnce,omitempty"`
	Traffic            []RevisionTrafficResponse `json:"traffic,omitempty"`
	Message            string                    `json:"message,omitempty"`
}

// CappStatusResponse is the status sub-object in a CappResponse.
// It exposes a flattened view of the condition tree that the frontend's
// Conditions table renders.
type CappStatusResponse struct {
	Conditions  []ConditionResponse `json:"conditions"`
	StateStatus StateStatusResponse `json:"stateStatus"`

	// Rollout is the state of the staged rollout. Nil when the operator reports none.
	Rollout *RolloutStatusResponse `json:"rollout,omitempty"`
}

// BareCappResponse contains the basic info of a Capp, without the status
type BareCappResponse struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	CreatedAt       string            `json:"createdAt,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`

	ScaleSpec     ScaleSpec     `json:"scaleSpec,omitempty"`
	State         string        `json:"state,omitempty"`
	Image         string        `json:"image,omitempty"`
	ContainerName string        `json:"containerName,omitempty"`
	Size          CappSize      `json:"size,omitempty"`
	Resources     *ResourceSpec `json:"resources,omitempty"`
	Env           []EnvVar      `json:"env,omitempty"`
	VolumeMounts  []VolumeMount `json:"volumeMounts,omitempty"`
	RouteSpec     *RouteSpec    `json:"routeSpec,omitempty"`
	LogSpec       *LogSpec      `json:"logSpec,omitempty"`
	NFSVolumes    []NFSVolume   `json:"nfsVolumes,omitempty"`

	SecretVolumes               []SecretVolume    `json:"secretVolumes,omitempty"`
	ConfigMapVolumes            []ConfigMapVolume `json:"configMapVolumes,omitempty"`
	ImagePullSecrets            []string          `json:"imagePullSecrets,omitempty"`
	ReloadOnBoundResourceChange bool              `json:"reloadOnBoundResourceChange,omitempty"`
}

// CappResponse is returned by all read and write endpoints.
type CappResponse struct {
	BareCappResponse
	Status CappStatusResponse `json:"status"`

	// Warnings reports side effects that failed while the requested cluster operation succeeded. for example, capp gitops backup operation failure.
	Warnings []Warning `json:"warnings,omitempty"`
}

// CappRevisionResponse is a historical snapshot of a Capp, recorded by the
// operator as a CappRevision on every spec, label or annotation change.
type CappRevisionResponse struct {
	BareCappResponse
	RevisionNumber int `json:"revisionNumber"`
}

// Warning is a non-fatal problem attached to a successful response.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type DeleteResponse struct {
	Warnings []Warning `json:"warnings,omitempty"`
}

// CappListResponse is the envelope returned by the list endpoints.
type CappListResponse struct {
	Items []CappResponse `json:"items"`
	Total int            `json:"total"`
}

// CappRevisionListResponse is the envelope returned by the revision list endpoint.
type CappRevisionListResponse struct {
	Items []CappRevisionResponse `json:"items"`
	Total int                    `json:"total"`
}

// ── Rollout types ─────────────────────────────────────────────────────────────

// RolloutActionRequest is the body for POST .../capps/:name/rollout. Revision
// must name the current candidate, which binds the action to one rollout.
type RolloutActionRequest struct {
	Action   string `json:"action"   binding:"required,oneof=promote abort retry"`
	Revision string `json:"revision" binding:"required"`
}

// ── Migration types ───────────────────────────────────────────────────────────

// MigrateRequest is the body for POST .../capps/:name/migrate.
type MigrateRequest struct {
	TargetCluster   string `json:"targetCluster"    binding:"required"`
	TargetNamespace string `json:"targetNamespace"  binding:"required"`
	DeleteSource    bool   `json:"deleteSource"`
	TargetHostname  string `json:"targetHostname,omitempty"`
}

// MigrateResponse is returned on successful migration.
type MigrateResponse struct {
	Name             string   `json:"name"`
	SourceCluster    string   `json:"sourceCluster"`
	SourceNamespace  string   `json:"sourceNamespace"`
	TargetCluster    string   `json:"targetCluster"`
	TargetNamespace  string   `json:"targetNamespace"`
	SourceDeleted    bool     `json:"sourceDeleted"`
	CopiedSecrets    []string `json:"copiedSecrets,omitempty"`
	CopiedConfigMaps []string `json:"copiedConfigMaps,omitempty"`
}

// SyncResponse is the JSON body returned by the sync endpoints.
type SyncResponse struct {
	Enabled   bool   `json:"enabled"`
	CommitSHA string `json:"commitSha,omitempty"`
	Path      string `json:"path,omitempty"`
}

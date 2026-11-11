// Package capps implements the Capp resource handler.
//
// It exposes full CRUD for the rcs.dana.io/v1alpha1/Capp custom resource:
//
//	GET    /api/v1/clusters/:cluster/capps                               (all namespaces)
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps
//	GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
//	POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps
//	PUT    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
//	DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
//
// The handler translates between the frontend-facing DTOs defined in this file
// and the Kubernetes Capp type via convert.go. All K8s API interaction happens
// through the per-request scoped client injected by the cluster middleware.
package capps

import api "github.com/dana-team/capp-backend/pkg/api/capps"

// The DTOs live in pkg/api/capps so clients can import them without this
// package's dependencies; these aliases keep the handler code unchanged.
type (
	CappSize                  = api.CappSize
	ResourceQuantities        = api.ResourceQuantities
	ResourceSpec              = api.ResourceSpec
	EnvVarSource              = api.EnvVarSource
	KeySelector               = api.KeySelector
	EnvVar                    = api.EnvVar
	VolumeMount               = api.VolumeMount
	RouteSpec                 = api.RouteSpec
	RolloutStage              = api.RolloutStage
	RolloutStrategy           = api.RolloutStrategy
	LogSpec                   = api.LogSpec
	NFSVolume                 = api.NFSVolume
	SecretVolume              = api.SecretVolume
	ConfigMapVolume           = api.ConfigMapVolume
	ScaleSpec                 = api.ScaleSpec
	CappRequest               = api.CappRequest
	PingSourceConfig          = api.PingSourceConfig
	KafkaSourceConfig         = api.KafkaSourceConfig
	SourceConfig              = api.SourceConfig
	EventSourcesSpec          = api.EventSourcesSpec
	ConditionResponse         = api.ConditionResponse
	EventSourceStatusResponse = api.EventSourceStatusResponse
	EventingStatusResponse    = api.EventingStatusResponse
	SyncResponse              = api.SyncResponse
	StateStatusResponse       = api.StateStatusResponse
	RevisionTrafficResponse   = api.RevisionTrafficResponse
	RolloutStatusResponse     = api.RolloutStatusResponse
	CappStatusResponse        = api.CappStatusResponse
	BareCappResponse          = api.BareCappResponse
	CappResponse              = api.CappResponse
	CappRevisionResponse      = api.CappRevisionResponse
	Warning                   = api.Warning
	DeleteResponse            = api.DeleteResponse
	CappListResponse          = api.CappListResponse
	CappRevisionListResponse  = api.CappRevisionListResponse
	RolloutActionRequest      = api.RolloutActionRequest
	MigrateRequest            = api.MigrateRequest
	MigrateResponse           = api.MigrateResponse
)

const (
	CappSizeSmall  = api.CappSizeSmall
	CappSizeMedium = api.CappSizeMedium
	CappSizeLarge  = api.CappSizeLarge
)

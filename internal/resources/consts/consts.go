package consts

const (
	ManagedLabelKey          = "dana.io/capp-managed"
	ManagedLabelValue        = "true"
	ManagedNameSpaceLabelKey = "dana.io/capp-ns"
	CappAPIGroup             = "rcs.dana.io"
	CappResource             = "capps"

	// BoundResourceRolloutAnnotation set to "true" on a Capp opts it in to a
	// restart when a Secret or ConfigMap it references is updated.
	BoundResourceRolloutAnnotation = CappAPIGroup + "/rollout-on-bound-resource-change"
	BoundResourceRolloutEnabled    = "true"

	// BoundResourceVersionsAnnotation records, as a JSON object of
	// "<Kind>/<name>" to resourceVersion, the version of each bound Secret or
	// ConfigMap the Capp was last restarted for. A new resourceVersion changes
	// the value, which restarts the Capp; a retry for a version already
	// recorded is a no-op. RestartedAtAnnotation and RestartReasonAnnotation
	// are informational and written alongside it.
	//
	// All three must stay outside the rcs.dana.io/ prefix: the operator copies
	// only non-rcs.dana.io Capp annotations into the Knative revision template,
	// and a template change is what creates a new revision.
	BoundResourceVersionsAnnotation = "capp.dana.io/bound-resource-versions"
	RestartedAtAnnotation           = "capp.dana.io/restarted-at"
	RestartReasonAnnotation         = "capp.dana.io/restart-reason"
)

package k8s

// LabelBackupToGit is set on a Capp that has been backed up to the GitOps
// repository. It marks the Capp as participating in GitOps sync and allows
// filtering backed-up Capps via label selectors.
const LabelBackupToGit = "rcs.dana.io/backup-to-git"

// LabelCappName is set by the operator on every CappRevision and holds the
// name of the Capp the revision belongs to.
const LabelCappName = "rcs.dana.io/cappName"

// AnnotationRollout requests a rollout action from the operator, as
// promote:<revision>, abort:<revision> or retry:<revision>. The operator
// removes it once processed. Its own constant is in an internal package.
const AnnotationRollout = "rcs.dana.io/rollout"

// HasBackupLabel reports whether the given label map contains the
// backup-to-git marker.
func HasBackupLabel(labels map[string]string) bool {
	return labels != nil && labels[LabelBackupToGit] == "true"
}

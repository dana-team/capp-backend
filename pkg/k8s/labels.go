package k8s

// LabelBackupToGit was set on a Capp by the removed Git sync. Nothing sets or
// reads it any more; desired state ignores it, so that removing the leftover
// label from a Capp is not drift.
const LabelBackupToGit = "rcs.dana.io/backup-to-git"

// LabelCappName is set by the operator on every CappRevision and holds the
// name of the Capp the revision belongs to.
const LabelCappName = "rcs.dana.io/cappName"

// AnnotationRollout requests a rollout action from the operator, as
// promote:<revision>, abort:<revision> or retry:<revision>. The operator
// removes it once processed. Its own constant is in an internal package.
const AnnotationRollout = "rcs.dana.io/rollout"

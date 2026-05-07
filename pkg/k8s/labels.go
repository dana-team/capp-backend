package k8s

// LabelBackupToGit is set on a Capp that has been backed up to the GitOps
// repository. It marks the Capp as participating in GitOps sync and allows
// filtering backed-up Capps via label selectors.
const LabelBackupToGit = "rcs.dana.io/backup-to-git"

// HasBackupLabel reports whether the given label map contains the
// backup-to-git marker.
func HasBackupLabel(labels map[string]string) bool {
	return labels != nil && labels[LabelBackupToGit] == "true"
}

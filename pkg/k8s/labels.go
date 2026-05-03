package k8s

// LabelBackupToGit is set on a Capp that has been backed up to the GitOps
// repository. Its presence tells the publish endpoint that the Capp is
// already saved and allows filtering backed-up Capps via label selectors.
const LabelBackupToGit = "capp.dana.io/backup-to-git"

// HasBackupLabel reports whether the given label map contains the
// backup-to-git marker.
func HasBackupLabel(labels map[string]string) bool {
	return labels != nil && labels[LabelBackupToGit] == "true"
}

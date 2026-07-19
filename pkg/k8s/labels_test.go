package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasBackupLabel_Present(t *testing.T) {
	labels := map[string]string{
		LabelBackupToGit: "true",
		"app":            "my-app",
	}
	assert.True(t, HasBackupLabel(labels))
}

func TestHasBackupLabel_Missing(t *testing.T) {
	labels := map[string]string{"app": "my-app"}
	assert.False(t, HasBackupLabel(labels))
}

func TestHasBackupLabel_NilMap(t *testing.T) {
	assert.False(t, HasBackupLabel(nil))
}

func TestHasBackupLabel_WrongValue(t *testing.T) {
	labels := map[string]string{LabelBackupToGit: "false"}
	assert.False(t, HasBackupLabel(labels))
}

func TestHasBackupLabel_EmptyMap(t *testing.T) {
	assert.False(t, HasBackupLabel(map[string]string{}))
}

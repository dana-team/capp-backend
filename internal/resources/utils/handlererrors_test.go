package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErrContextMissing_Error(t *testing.T) {
	err := ErrContextMissing("K8sClientKey")
	assert.Contains(t, err.Error(), "K8sClientKey")
	assert.Contains(t, err.Error(), "check middleware order")
}

func TestErrContextMissing_ReturnsDistinctInstances(t *testing.T) {
	err1 := ErrContextMissing("key1")
	err2 := ErrContextMissing("key2")
	assert.NotEqual(t, err1.Error(), err2.Error())
}

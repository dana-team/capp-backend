package resources

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type mockHandler struct {
	name          string
	registerCalls int
}

func (m *mockHandler) Name() string { return m.name }
func (m *mockHandler) RegisterRoutes(_ *gin.RouterGroup) {
	m.registerCalls++
}

// -- Registry tests --

func TestNewRegistry_ReturnsNonNil(t *testing.T) {
	r := NewRegistry(nil)
	require.NotNil(t, r)
}

func TestRegister_EnabledHandler_Added(t *testing.T) {
	r := NewRegistry(map[string]bool{"capps": true})
	r.Register(&mockHandler{name: "capps"})
	assert.Len(t, r.handlers, 1)
}

func TestRegister_DisabledHandler_Skipped(t *testing.T) {
	r := NewRegistry(map[string]bool{"capps": false})
	r.Register(&mockHandler{name: "capps"})
	assert.Empty(t, r.handlers)
}

func TestRegister_MissingFromMap_StillAdded(t *testing.T) {
	r := NewRegistry(map[string]bool{})
	r.Register(&mockHandler{name: "namespaces"})
	assert.Len(t, r.handlers, 1)
}


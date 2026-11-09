package cluster

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ClusterMeta is returned by the cluster list API and the MCP tools.
func TestClusterMeta_JSONOmitsBenchmarkSAToken(t *testing.T) {
	out, err := json.Marshal(ClusterMeta{Name: "c1", BenchmarkSAToken: "sa-token-value"})
	require.NoError(t, err)
	assert.NotContains(t, string(out), "sa-token-value")
}

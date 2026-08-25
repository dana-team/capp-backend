package auth

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/root"
)

func TestMCPHeadersCommand(t *testing.T) {
	state := &root.State{Token: "fresh-token"}

	cmd := NewMCPHeadersCommand(state)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)

	require.NoError(t, cmd.Execute())

	var headers map[string]string
	require.NoError(t, json.Unmarshal(buf.Bytes(), &headers))
	require.Equal(t, "Bearer fresh-token", headers["Authorization"])
}

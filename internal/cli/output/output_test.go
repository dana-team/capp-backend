package output_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/output"
)

type item struct {
	Name  string
	Image string
	UID   string
}

func cols() []output.Column[item] {
	return []output.Column[item]{
		{Header: "NAME", Value: func(i item) string { return i.Name }},
		{Header: "IMAGE", Value: func(i item) string { return i.Image }},
		{Header: "UID", Value: func(i item) string { return i.UID }, Wide: true},
	}
}

func TestTableOutput(t *testing.T) {
	items := []item{{Name: "myapp", Image: "nginx:latest", UID: "uid-123"}}
	var buf bytes.Buffer
	output.PrintTable(&buf, cols(), items, false)
	out := buf.String()
	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "IMAGE")
	assert.Contains(t, out, "myapp")
	assert.Contains(t, out, "nginx:latest")
	assert.NotContains(t, out, "UID")
	assert.NotContains(t, out, "uid-123")
}

func TestWideOutput(t *testing.T) {
	items := []item{{Name: "myapp", Image: "nginx:latest", UID: "uid-123"}}
	var buf bytes.Buffer
	output.PrintTable(&buf, cols(), items, true)
	out := buf.String()
	assert.Contains(t, out, "UID")
	assert.Contains(t, out, "uid-123")
}

func TestEmptyTable(t *testing.T) {
	var buf bytes.Buffer
	output.PrintTable(&buf, cols(), []item{}, false)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Len(t, lines, 1, "empty table should have header only")
}

func TestJSONOutput(t *testing.T) {
	data := map[string]string{"name": "myapp"}
	var buf bytes.Buffer
	require.NoError(t, output.PrintJSON(&buf, data))
	var decoded map[string]string
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "myapp", decoded["name"])
}

func TestYAMLOutput(t *testing.T) {
	data := map[string]string{"name": "myapp"}
	var buf bytes.Buffer
	require.NoError(t, output.PrintYAML(&buf, data))
	assert.Contains(t, buf.String(), "name: myapp")
}

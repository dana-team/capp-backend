package mcpserver

import "github.com/modelcontextprotocol/go-sdk/mcp"

// ToolSet registers one related group of MCP tools.
type ToolSet interface {
	Name() string

	// Register attaches this tool set's tools to s.
	Register(s *mcp.Server, be *Backend)
}

// Registry holds the enabled ToolSets and mounts them onto a MCP Server.
type Registry struct {
	sets    []ToolSet
	enabled map[string]bool
}

// NewRegistry creates Registry. enabled maps a ToolSet's Name() to
// whether it should be registered; a name absent from the map is enabled by default.
func NewRegistry(enabled map[string]bool) *Registry {
	return &Registry{enabled: enabled}
}

// Register adds a ToolSet to the registry.
func (r *Registry) Register(ts ToolSet) {
	if enabled, ok := r.enabled[ts.Name()]; ok && !enabled {
		return
	}
	r.sets = append(r.sets, ts)
}

// Mount calls Register on every registered ToolSet, attaching their tools to s.
func (r *Registry) Mount(s *mcp.Server, be *Backend) {
	for _, ts := range r.sets {
		ts.Register(s, be)
	}
}

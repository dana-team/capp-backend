package mcpserver

import "github.com/modelcontextprotocol/go-sdk/mcp"

type Config struct {
	Backend Backend

	// Enabled selectively disables tool sets by name (clusters, namespaces,
	// capps, configmaps, secrets). A name absent from the map is enabled by default.
	Enabled map[string]bool
}

// NewServer builds the shared MCP server with every enabled tool set
// registered. The same *mcp.Server instance is reused for every MCP session.
func NewServer(cfg Config) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "capp-mcp",
		Version: "0.1.0",
	}, &mcp.ServerOptions{
		Instructions: "Tools for inspecting and managing Capp resources, namespaces, and ConfigMaps across the clusters known to capp-backend. Every tool call runs with the caller's own bearer token against capp-backend's REST API, so results are scoped by the same RBAC the API already enforces. Secret tools are read-only and only ever return metadata (name, type, labels, data key names) — secret values are never exposed here.",
	})

	registry := NewRegistry(cfg.Enabled)
	registry.Register(ClusterToolSet{})
	registry.Register(NamespaceToolSet{})
	registry.Register(CappToolSet{})
	registry.Register(ConfigMapToolSet{})
	registry.Register(SecretToolSet{})
	registry.Mount(s, &cfg.Backend)

	return s
}

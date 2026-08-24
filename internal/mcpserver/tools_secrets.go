package mcpserver

import (
	"context"
	"fmt"
	"sort"

	"github.com/dana-team/capp-backend/internal/resources/namespaced/secrets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SecretToolSet exposes read-only metadata for capp-managed Secrets
// (labeled dana.io/capp-managed=true). Secret values are never returned;
// only data key names are exposed. Mutating tools are intentionally omitted.
type SecretToolSet struct{}

func (SecretToolSet) Name() string { return "secrets" }

// secretMeta is the scrubbed, MCP-facing view of a secrets.SecretResponse:
// every field except the actual data values, which are reduced to key names.
type secretMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Type            string            `json:"type"`
	CreatedAt       string            `json:"createdAt,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Keys            []string          `json:"keys,omitempty" jsonschema:"names of the keys stored in the secret's data — values are never exposed"`
}

func toSecretMeta(in secrets.SecretResponse) secretMeta {
	keys := make([]string, 0, len(in.Data))
	for k := range in.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return secretMeta{
		Name:            in.Name,
		Namespace:       in.Namespace,
		Type:            in.Type,
		CreatedAt:       in.CreatedAt,
		UID:             in.UID,
		ResourceVersion: in.ResourceVersion,
		Labels:          in.Labels,
		Annotations:     in.Annotations,
		Keys:            keys,
	}
}

type secretListOutput struct {
	Items []secretMeta `json:"items"`
	Total int          `json:"total"`
}

type secretListInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace,omitempty" jsonschema:"optional: restrict the list to this namespace; omit to list across every namespace on the cluster"`
}

type secretGetInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the secret"`
	Name      string `json:"name" jsonschema:"the secret name"`
}

func (SecretToolSet) Register(s *mcp.Server, be *Backend) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "secret_list",
		Description: "List capp-managed Secrets on a cluster, optionally restricted to one namespace. Returns metadata only (name, type, labels, data key names, etc.) — secret values are never included.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in secretListInput) (*mcp.CallToolResult, secretListOutput, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, secretListOutput{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/secrets", in.Cluster)
		if in.Namespace != "" {
			path = fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/secrets", in.Cluster, in.Namespace)
		}
		var raw secrets.SecretListResponse
		if err := c.Get(ctx, path, &raw); err != nil {
			return nil, secretListOutput{}, err
		}
		out := secretListOutput{Items: make([]secretMeta, len(raw.Items)), Total: raw.Total}
		for i, item := range raw.Items {
			out.Items[i] = toSecretMeta(item)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "secret_get",
		Description: "Get metadata for one capp-managed Secret by name (name, type, labels, data key names, etc.). Secret values are never included.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in secretGetInput) (*mcp.CallToolResult, secretMeta, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, secretMeta{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/secrets/%s", in.Cluster, in.Namespace, in.Name)
		var raw secrets.SecretResponse
		if err := c.Get(ctx, path, &raw); err != nil {
			return nil, secretMeta{}, err
		}
		return nil, toSecretMeta(raw), nil
	})
}

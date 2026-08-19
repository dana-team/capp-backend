package mcpserver

import (
	"context"
	"fmt"

	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConfigMapToolSet exposes CRUD for capp-managed ConfigMaps (those labeled
// dana.io/capp-managed=true), mirroring the backend's configmaps.Handler routes.
type ConfigMapToolSet struct{}

func (ConfigMapToolSet) Name() string { return "configmaps" }

type configMapListInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace,omitempty" jsonschema:"optional: restrict the list to this namespace; omit to list across every namespace on the cluster"`
}

type configMapGetInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the config map"`
	Name      string `json:"name" jsonschema:"the config map name"`
}

type configMapCreateInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace to create the config map in"`
	configmaps.ConfigMapRequest
}

type configMapUpdateInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the config map"`
	Name      string `json:"name" jsonschema:"the config map name to update"`
	configmaps.ConfigMapUpdateRequest
}

type configMapDeleteInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the config map"`
	Name      string `json:"name" jsonschema:"the config map name to delete"`
}

type configMapDeleteOutput struct {
	Deleted   bool   `json:"deleted"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (ConfigMapToolSet) Register(s *mcp.Server, be *Backend) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "configmap_list",
		Description: "List capp-managed ConfigMaps on a cluster, optionally restricted to one namespace.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in configMapListInput) (*mcp.CallToolResult, configmaps.ConfigMapListResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, configmaps.ConfigMapListResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/configmaps", in.Cluster)
		if in.Namespace != "" {
			path = fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/configmaps", in.Cluster, in.Namespace)
		}
		var out configmaps.ConfigMapListResponse
		if err := c.Get(ctx, path, &out); err != nil {
			return nil, configmaps.ConfigMapListResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "configmap_get",
		Description: "Get one capp-managed ConfigMap by name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in configMapGetInput) (*mcp.CallToolResult, configmaps.ConfigMapResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, configmaps.ConfigMapResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/configmaps/%s", in.Cluster, in.Namespace, in.Name)
		var out configmaps.ConfigMapResponse
		if err := c.Get(ctx, path, &out); err != nil {
			return nil, configmaps.ConfigMapResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "configmap_create",
		Description: "Create a ConfigMap in a namespace. It is automatically labeled dana.io/capp-managed=true so it shows up in configmap_list/configmap_get.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in configMapCreateInput) (*mcp.CallToolResult, configmaps.ConfigMapResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, configmaps.ConfigMapResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/configmaps", in.Cluster, in.Namespace)
		var out configmaps.ConfigMapResponse
		if err := c.Post(ctx, path, in.ConfigMapRequest, &out); err != nil {
			return nil, configmaps.ConfigMapResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "configmap_update",
		Description: "Replace a ConfigMap's data. This is a full replace of the data map, not a merge — keys you omit are removed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in configMapUpdateInput) (*mcp.CallToolResult, configmaps.ConfigMapResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, configmaps.ConfigMapResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/configmaps/%s", in.Cluster, in.Namespace, in.Name)
		var out configmaps.ConfigMapResponse
		if err := c.Put(ctx, path, in.ConfigMapUpdateRequest, &out); err != nil {
			return nil, configmaps.ConfigMapResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "configmap_delete",
		Description: "Delete a ConfigMap. This is irreversible.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in configMapDeleteInput) (*mcp.CallToolResult, configMapDeleteOutput, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, configMapDeleteOutput{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/configmaps/%s", in.Cluster, in.Namespace, in.Name)
		if err := c.Delete(ctx, path); err != nil {
			return nil, configMapDeleteOutput{}, err
		}
		return nil, configMapDeleteOutput{Deleted: true, Cluster: in.Cluster, Namespace: in.Namespace, Name: in.Name}, nil
	})
}

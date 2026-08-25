package mcpserver

import (
	"context"
	"fmt"

	ns "github.com/dana-team/capp-backend/internal/resources/cluster/namespaces"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NamespaceToolSet exposes the namespace lifecycle: list, create, and the two
// update flavors capp-backend supports (full-replace vs. additive-users).
type NamespaceToolSet struct{}

func (NamespaceToolSet) Name() string { return "namespaces" }

type namespaceListInput struct {
	Cluster string `json:"cluster" jsonschema:"the target cluster name"`
}

type namespaceCreateInput struct {
	Cluster string `json:"cluster" jsonschema:"the target cluster name"`
	ns.CreateNamespaceRequest
}

type namespaceUpdateInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace to update"`
	ns.UpdateNamespaceRequest
}

// namespaceAddUsersInput intentionally does not embed ns.PatchNamespaceRequest:
// Users is required here (the tool's sole purpose), but PatchNamespaceRequest
// declares it as an optional pointer, which would let a caller omit it and
// silently no-op.
type namespaceAddUsersInput struct {
	Cluster   string   `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string   `json:"namespace" jsonschema:"the namespace to add users to"`
	Users     []string `json:"users" jsonschema:"usernames to grant the capp-user role in this namespace; existing users are kept, not replaced"`
}

func (NamespaceToolSet) Register(s *mcp.Server, be *Backend) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "namespace_list",
		Description: "List CAPP-enabled namespaces the caller can access on a cluster, and whether the caller can create new ones.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in namespaceListInput) (*mcp.CallToolResult, ns.NamespaceListResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, ns.NamespaceListResponse{}, err
		}
		var out ns.NamespaceListResponse
		if err := c.Get(ctx, fmt.Sprintf("/api/v1/clusters/%s/namespaces", in.Cluster), &out); err != nil {
			return nil, ns.NamespaceListResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "namespace_get",
		Description: "Get a CAPP-enabled namespace's details, including its resource quota and list of users granted the capp-user role.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in namespaceAddUsersInput) (*mcp.CallToolResult, ns.NamespaceItem, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		var out ns.NamespaceItem
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s", in.Cluster, in.Namespace)
		if err := c.Get(ctx, path, &out); err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		return nil, out, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "namespace_create",
		Description: "Create a CAPP-enabled namespace, optionally with a resource quota and an initial set of users granted the capp-user role. Requires the caller to have permission to create namespaces on the cluster.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in namespaceCreateInput) (*mcp.CallToolResult, ns.NamespaceItem, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		var out ns.NamespaceItem
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces", in.Cluster)
		if err := c.Post(ctx, path, in.CreateNamespaceRequest, &out); err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "namespace_update",
		Description: "Replace a namespace's resource quota and/or user list. This is a full replace, not additive: a provided users list replaces the existing one entirely, and quota decreases below current usage are rejected. Use namespace_add_users to append users without replacing the list.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in namespaceUpdateInput) (*mcp.CallToolResult, ns.NamespaceItem, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		var out ns.NamespaceItem
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s", in.Cluster, in.Namespace)
		if err := c.Put(ctx, path, in.UpdateNamespaceRequest, &out); err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "namespace_add_users",
		Description: "Add users to a namespace's existing capp-user RoleBinding without disturbing users already granted access. The RoleBinding must already exist (created via namespace_create) or this fails with a not-found error.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in namespaceAddUsersInput) (*mcp.CallToolResult, ns.NamespaceItem, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		var out ns.NamespaceItem
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s", in.Cluster, in.Namespace)
		body := ns.PatchNamespaceRequest{Users: &in.Users}
		if err := c.Patch(ctx, path, body, &out); err != nil {
			return nil, ns.NamespaceItem{}, err
		}
		return nil, out, nil
	})
}

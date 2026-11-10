package mcpserver

import (
	"context"
	"fmt"

	ns "github.com/dana-team/capp-backend/internal/resources/cluster/namespaces"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NamespaceToolSet exposes the namespace lifecycle: list, create, delete, and the two
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

type namespaceDeleteInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace to delete"`
}

type namespaceDeleteOutput struct {
	Deleted   bool   `json:"deleted"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
}

// namespaceAddUsersInput intentionally does not embed ns.PatchNamespaceRequest:
// Users is required here (the tool's sole purpose), but PatchNamespaceRequest
// declares it as an optional pointer, which would let a caller omit it and
// silently no-op.
type namespaceAddUsersInput struct {
	Cluster   string   `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string   `json:"namespace" jsonschema:"the namespace to add users to"`
	Users     []string `json:"users" jsonschema:"usernames to grant the capp-user role in Kubernetes authorization mode or direct editor access in backend-policy mode; existing grants are kept"`
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
		Description: "Get a CAPP-enabled namespace's details and resource quota. Users lists capp-user RoleBinding users in Kubernetes authorization mode, or direct editors in backend-policy mode, which also returns full policy bindings when available.",
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
		Description: "Create a CAPP-enabled namespace with optional quota and membership. Kubernetes authorization mode grants users the capp-user role and requires namespace creation permission. Backend-policy mode requires a configured operator; supply users for direct editor grants or bindings for explicit User/Group viewer/editor grants, never both.",
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
		Name:        "namespace_delete",
		Description: "Delete a CAPP-enabled namespace. If it still contains Capps, the server rejects the request with a conflict error. Backend-policy mode requires a configured operator and also cleans up the owned policy; retries while the namespace is terminating can finish policy cleanup. Namespace deletion is irreversible.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in namespaceDeleteInput) (*mcp.CallToolResult, namespaceDeleteOutput, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, namespaceDeleteOutput{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s", in.Cluster, in.Namespace)
		if err := c.Delete(ctx, path, nil); err != nil {
			return nil, namespaceDeleteOutput{}, err
		}
		return nil, namespaceDeleteOutput{Deleted: true, Cluster: in.Cluster, Namespace: in.Namespace}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "namespace_update",
		Description: "Update a namespace's quota and membership; omitted fields stay unchanged and quota decreases below usage are rejected. Kubernetes authorization mode replaces capp-user RoleBinding users. Backend-policy mode requires a configured operator: users replaces only direct editors, preserving viewers and Group grants; bindings replaces all grants. Supply users or bindings, never both; policyResourceVersion detects stale policy edits. Use namespace_add_users to append users.",
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
		Description: "Add users without removing existing grants. Kubernetes authorization mode requires an existing capp-user RoleBinding. Backend-policy mode requires a configured operator and an existing valid policy, and adds direct editor grants. Use namespace_update to initialize missing membership or repair an invalid policy with full bindings in backend-policy mode.",
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

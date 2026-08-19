package mcpserver

import (
	"context"
	"fmt"

	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CappToolSet exposes full CRUD plus sync for the Capp custom resource,
// mirroring the backend's own capps.Handler routes.
type CappToolSet struct{}

func (CappToolSet) Name() string { return "capps" }

type cappListInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace,omitempty" jsonschema:"optional: restrict the list to this namespace; omit to list Capps across every namespace on the cluster"`
}

type cappGetInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name"`
}

type cappCreateInput struct {
	Cluster string `json:"cluster" jsonschema:"the target cluster name"`
	capps.CappRequest
}

type cappUpdateInput struct {
	Cluster string `json:"cluster" jsonschema:"the target cluster name"`
	capps.CappRequest
}

type cappDeleteInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name to delete"`
}

type cappDeleteOutput struct {
	Deleted   bool   `json:"deleted"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type cappSyncInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name to sync"`
}

func (CappToolSet) Register(s *mcp.Server, be *Backend) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_list",
		Description: "List Capps on a cluster, optionally restricted to one namespace.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappListInput) (*mcp.CallToolResult, capps.CappListResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappListResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/capps", in.Cluster)
		if in.Namespace != "" {
			path = fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps", in.Cluster, in.Namespace)
		}
		var out capps.CappListResponse
		if err := c.Get(ctx, path, &out); err != nil {
			return nil, capps.CappListResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_get",
		Description: "Get one Capp by name, including its resolved resources and status conditions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappGetInput) (*mcp.CallToolResult, capps.CappResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", in.Cluster, in.Namespace, in.Name)
		var out capps.CappResponse
		if err := c.Get(ctx, path, &out); err != nil {
			return nil, capps.CappResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_create",
		Description: "Create a new Capp. Set either size (small|medium|large) or customResources, not both.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappCreateInput) (*mcp.CallToolResult, capps.CappResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps", in.Cluster, in.Namespace)
		var out capps.CappResponse
		if err := c.Post(ctx, path, in.CappRequest, &out); err != nil {
			return nil, capps.CappResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_update",
		Description: "Replace an existing Capp's spec. This is a full replacement of everything capp_get returns as editable fields, not a partial patch — fields you omit are cleared, not left as-is. Call capp_get first and send back the full desired state.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappUpdateInput) (*mcp.CallToolResult, capps.CappResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", in.Cluster, in.Namespace, in.Name)
		var out capps.CappResponse
		if err := c.Put(ctx, path, in.CappRequest, &out); err != nil {
			return nil, capps.CappResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_delete",
		Description: "Delete a Capp. This is irreversible.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappDeleteInput) (*mcp.CallToolResult, cappDeleteOutput, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, cappDeleteOutput{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", in.Cluster, in.Namespace, in.Name)
		if err := c.Delete(ctx, path); err != nil {
			return nil, cappDeleteOutput{}, err
		}
		return nil, cappDeleteOutput{Deleted: true, Cluster: in.Cluster, Namespace: in.Namespace, Name: in.Name}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_sync",
		Description: "Push the Capp's current Helm values to the GitOps repository and mark it backed-up-to-git. Returns a not-supported error if GitOps is disabled on this capp-backend deployment.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappSyncInput) (*mcp.CallToolResult, capps.SyncResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.SyncResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/sync", in.Cluster, in.Namespace, in.Name)
		var out capps.SyncResponse
		if err := c.Post(ctx, path, nil, &out); err != nil {
			return nil, capps.SyncResponse{}, err
		}
		return nil, out, nil
	})
}

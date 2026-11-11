package mcpserver

import (
	"context"
	"fmt"

	"github.com/dana-team/capp-backend/pkg/api/capps"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CappToolSet exposes full CRUD plus sync, migrate and rollout actions for the Capp custom
// resource, mirroring the backend's own capps.Handler routes.
type CappToolSet struct{}

// cappUpdateDescription is the capp_update tool description.
const cappUpdateDescription = "Replace an existing Capp's spec. This is a full replacement of everything capp_get returns as editable fields, not a partial patch — fields you omit are cleared, not left as-is. The exception is routeSpec.rolloutStrategy: omitting it keeps the current strategy; use capp_rollout_strategy_remove to remove one. Call capp_get first and send back the full desired state. If Git sync is enabled, the change is also committed to git."

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
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace where the capp should be created"`
	capps.CappRequest
}

type cappUpdateInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
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

type cappMigrateInput struct {
	Cluster   string `json:"cluster" jsonschema:"the source cluster name"`
	Namespace string `json:"namespace" jsonschema:"the source namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name to migrate"`
	capps.MigrateRequest
}

type cappRolloutInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name whose rollout to act on"`
	capps.RolloutActionRequest
}

type cappRolloutStrategyRemoveInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name whose rollout strategy to remove"`
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
		Description: cappUpdateDescription,
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
		Description: "Delete a Capp. This is irreversible. If Git sync is enabled, its values file is also deleted.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappDeleteInput) (*mcp.CallToolResult, cappDeleteOutput, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, cappDeleteOutput{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", in.Cluster, in.Namespace, in.Name)
		if err := c.Delete(ctx, path, nil); err != nil {
			return nil, cappDeleteOutput{}, err
		}
		return nil, cappDeleteOutput{Deleted: true, Cluster: in.Cluster, Namespace: in.Namespace, Name: in.Name}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_sync",
		Description: "Enable Git sync for a Capp (or re-sync it). Updates are then committed to the GitOps repository automatically.",
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

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_sync_disable",
		Description: "Disable Git sync for a Capp. Deletes its values file from the GitOps repository.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappSyncInput) (*mcp.CallToolResult, capps.SyncResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.SyncResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/sync", in.Cluster, in.Namespace, in.Name)
		var out capps.SyncResponse
		if err := c.Delete(ctx, path, &out); err != nil {
			return nil, capps.SyncResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_migrate",
		Description: "Migrate a Capp and its managed Secrets/ConfigMaps to a different cluster or namespace. When deleteSource is true the original Capp is removed after a successful copy — this is irreversible.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappMigrateInput) (*mcp.CallToolResult, capps.MigrateResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.MigrateResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/migrate", in.Cluster, in.Namespace, in.Name)
		var out capps.MigrateResponse
		if err := c.Post(ctx, path, in.MigrateRequest, &out); err != nil {
			return nil, capps.MigrateResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_rollout",
		Description: "Act on a Capp's staged rollout. action is one of promote (send the candidate 100% of traffic), abort (return all traffic to the stable revision) or retry (restart a Failed, RolledBack or Aborted rollout, or one held by a strategy change). revision must be the current status.rollout.candidateRevision from capp_get. The operator applies the action asynchronously, so check status.rollout with capp_get for the result. An action the status shows cannot apply returns a conflict. A promote of a candidate that is not Ready is accepted, then rejected only by a Warning event on the Capp.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappRolloutInput) (*mcp.CallToolResult, capps.CappResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/rollout", in.Cluster, in.Namespace, in.Name)
		var out capps.CappResponse
		if err := c.Post(ctx, path, in.RolloutActionRequest, &out); err != nil {
			return nil, capps.CappResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "capp_rollout_strategy_remove",
		Description: "Remove a Capp's rollout strategy. This moves live traffic: a Ready candidate that is rolling out is promoted to 100%, otherwise traffic is pinned to the stable revision until a newer revision is Ready. To stop a rollout and keep the strategy, use capp_rollout with action abort. If Git sync is enabled, the change is also committed to git.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappRolloutStrategyRemoveInput) (*mcp.CallToolResult, capps.CappResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/rollout/strategy", in.Cluster, in.Namespace, in.Name)
		var out capps.CappResponse
		if err := c.Delete(ctx, path, &out); err != nil {
			return nil, capps.CappResponse{}, err
		}
		return nil, out, nil
	})
}

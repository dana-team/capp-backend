package mcpserver

import (
	"context"
	"fmt"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ClusterToolSet exposes read-only discovery tools: which clusters exist
// and their health.
// These never mutate anything, so they are always safe to call first.
type ClusterToolSet struct{}

func (ClusterToolSet) Name() string { return "clusters" }

type clusterListOutput struct {
	Items []cluster.ClusterMeta `json:"items"`
}

type clusterGetInput struct {
	Cluster string `json:"cluster" jsonschema:"the cluster name, as returned by cluster_list"`
}

func (ClusterToolSet) Register(s *mcp.Server, be *Backend) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_list",
		Description: "List every cluster capp-backend is configured to manage, with health and OpenShift-vs-vanilla-Kubernetes metadata. Call this first to discover valid values for the cluster argument every other tool requires.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, clusterListOutput, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, clusterListOutput{}, err
		}
		var out clusterListOutput
		if err := c.Get(ctx, "/api/v1/clusters", &out); err != nil {
			return nil, clusterListOutput{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_get",
		Description: "Get health and metadata for a single cluster by name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterGetInput) (*mcp.CallToolResult, cluster.ClusterMeta, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, cluster.ClusterMeta{}, err
		}
		var out cluster.ClusterMeta
		if err := c.Get(ctx, fmt.Sprintf("/api/v1/clusters/%s", in.Cluster), &out); err != nil {
			return nil, cluster.ClusterMeta{}, err
		}
		return nil, out, nil
	})
}

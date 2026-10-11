package mcpserver

import (
	"context"
	"fmt"

	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CappRevisionToolSet exposes read access to a Capp's revision history and
// rollback, mirroring the backend's capps revisions routes.
type CappRevisionToolSet struct{}

func (CappRevisionToolSet) Name() string { return "capprevisions" }

type cappRevisionListInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name whose revisions to list"`
}

type cappRevisionGetInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name that owns the revision"`
	Revision  int    `json:"revision" jsonschema:"the revision number to fetch"`
}

type cappRollbackInput struct {
	Cluster   string `json:"cluster" jsonschema:"the target cluster name"`
	Namespace string `json:"namespace" jsonschema:"the namespace containing the capp"`
	Name      string `json:"name" jsonschema:"the capp name to roll back"`
	Revision  int    `json:"revision" jsonschema:"the revision number to roll the capp back to"`
}

func (CappRevisionToolSet) Register(s *mcp.Server, be *Backend) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_capp_revisions",
		Description: "List the revision history of a Capp, newest first. Each revision is a snapshot of the Capp's spec, labels and annotations recorded by the operator on every change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappRevisionListInput) (*mcp.CallToolResult, capps.CappRevisionListResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappRevisionListResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/revisions", in.Cluster, in.Namespace, in.Name)
		var out capps.CappRevisionListResponse
		if err := c.Get(ctx, path, &out); err != nil {
			return nil, capps.CappRevisionListResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_capp_revision",
		Description: "Get one revision of a Capp by its revision number, including the spec, labels and annotations captured in that revision.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappRevisionGetInput) (*mcp.CallToolResult, capps.CappRevisionResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappRevisionResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/revisions/%d", in.Cluster, in.Namespace, in.Name, in.Revision)
		var out capps.CappRevisionResponse
		if err := c.Get(ctx, path, &out); err != nil {
			return nil, capps.CappRevisionResponse{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "rollback_capp",
		Description: "Roll a Capp back to a previous revision. This overwrites the live Capp's spec, labels and annotations with that revision's values; the operator then records the result as a new revision. If Git sync is enabled, the rolled-back state is also committed to git.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cappRollbackInput) (*mcp.CallToolResult, capps.CappResponse, error) {
		c, err := be.Client(ctx)
		if err != nil {
			return nil, capps.CappResponse{}, err
		}
		path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/revisions/%d/rollback", in.Cluster, in.Namespace, in.Name, in.Revision)
		var out capps.CappResponse
		if err := c.Post(ctx, path, nil, &out); err != nil {
			return nil, capps.CappResponse{}, err
		}
		return nil, out, nil
	})
}

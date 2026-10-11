// Package capprevisions implements the cappctl command for listing and
// inspecting the revision history of a Capp.
package capprevisions

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/output"
	"github.com/dana-team/capp-backend/internal/cli/resource"
	"github.com/dana-team/capp-backend/internal/cli/root"
	"github.com/dana-team/capp-backend/internal/cli/utils"
	apitypes "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
)

const (
	outputJSON = "json"
	outputYAML = "yaml"
)

// ── Table column definitions ──────────────────────────────────────────────────

var tableCols = []output.Column[apitypes.CappRevisionResponse]{
	{Header: "REVISION", Value: func(r apitypes.CappRevisionResponse) string { return strconv.Itoa(r.RevisionNumber) }},
	{Header: "NAME", Value: func(r apitypes.CappRevisionResponse) string { return r.Name }},
	{Header: "IMAGE", Value: func(r apitypes.CappRevisionResponse) string { return r.Image }},
	{Header: "SIZE", Value: func(r apitypes.CappRevisionResponse) string { return string(r.Size) }},
	{Header: "STATE", Value: func(r apitypes.CappRevisionResponse) string { return r.State }},
	{Header: "AGE", Value: func(r apitypes.CappRevisionResponse) string { return utils.Age(r.CreatedAt) }},
	{Header: "CONTAINER", Value: func(r apitypes.CappRevisionResponse) string { return r.ContainerName }, Wide: true},
}

// ── Handler ───────────────────────────────────────────────────────────────────

type handler struct{ state *root.State }

// New returns a ResourceCommand for Capp revisions. Only get is supported.
func New(state *root.State) resource.ResourceCommand { return &handler{state: state} }

func (h *handler) Name() string      { return "capprevisions" }
func (h *handler) Aliases() []string { return []string{"capprevision", "revisions", "rev"} }

func (h *handler) RegisterGetCommand(parent *cobra.Command) {
	var capp string

	cmd := &cobra.Command{
		Use:     "capprevisions [revision]",
		Aliases: h.Aliases(),
		Short:   "List a Capp's revisions or get one by revision number",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := h.state.Cluster
			ns := h.state.Namespace
			if cluster == "" {
				return fmt.Errorf("--cluster is required (or set it in your context)")
			}
			if ns == "" {
				return fmt.Errorf("--namespace is required (or set it in your context)")
			}
			if capp == "" {
				return fmt.Errorf("--capp is required")
			}

			base := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/revisions", cluster, ns, capp)

			if len(args) == 1 {
				if n, err := strconv.Atoi(args[0]); err != nil || n <= 0 {
					return fmt.Errorf("invalid revision %q: must be a positive integer", args[0])
				}
				var item apitypes.CappRevisionResponse
				if err := h.state.Client.Get(cmd.Context(), base+"/"+args[0], &item); err != nil {
					return err
				}
				return h.render(cmd.OutOrStdout(), []apitypes.CappRevisionResponse{item}, item)
			}

			var list apitypes.CappRevisionListResponse
			if err := h.state.Client.Get(cmd.Context(), base, &list); err != nil {
				return err
			}
			return h.render(cmd.OutOrStdout(), list.Items, list)
		},
	}

	cmd.Flags().StringVar(&capp, "capp", "", "name of the Capp whose revisions to show (required)")
	_ = cmd.MarkFlagRequired("capp")
	parent.AddCommand(cmd)
}

func (h *handler) RegisterCreateCommand(parent *cobra.Command)   {}
func (h *handler) RegisterUpdateCommand(parent *cobra.Command)   {}
func (h *handler) RegisterDeleteCommand(parent *cobra.Command)   {}
func (h *handler) RegisterSyncCommand(parent *cobra.Command)     {}
func (h *handler) RegisterMigrateCommand(parent *cobra.Command)  {}
func (h *handler) RegisterRollbackCommand(parent *cobra.Command) {}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *handler) render(w io.Writer, items []apitypes.CappRevisionResponse, raw any) error {
	switch h.state.OutputFmt {
	case outputJSON:
		return output.PrintJSON(w, raw)
	case outputYAML:
		return output.PrintYAML(w, raw)
	case "wide":
		output.PrintTable(w, tableCols, items, true)
	default:
		output.PrintTable(w, tableCols, items, false)
	}
	return nil
}

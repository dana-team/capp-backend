package capps

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/output"
	"github.com/dana-team/capp-backend/internal/cli/resource"
	"github.com/dana-team/capp-backend/internal/cli/root"
	apitypes "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
)

// ── Table column definitions ──────────────────────────────────────────────────

var tableCols = []output.Column[apitypes.CappResponse]{
	{Header: "NAME", Value: func(c apitypes.CappResponse) string { return c.Name }},
	{Header: "NAMESPACE", Value: func(c apitypes.CappResponse) string { return c.Namespace }},
	{Header: "IMAGE", Value: func(c apitypes.CappResponse) string { return c.Image }},
	{Header: "STATE", Value: func(c apitypes.CappResponse) string { return c.State }},
	{Header: "METRIC", Value: func(c apitypes.CappResponse) string { return c.ScaleSpec.Metric }},
	{Header: "AGE", Value: func(c apitypes.CappResponse) string { return age(c.CreatedAt) }},
	{Header: "UID", Value: func(c apitypes.CappResponse) string { return c.UID }, Wide: true},
}

// ── Handler ───────────────────────────────────────────────────────────────────

type handler struct{ state *root.State }

// New returns a ResourceCommand for Capps.
func New(state *root.State) resource.ResourceCommand { return &handler{state: state} }

func (h *handler) Name() string      { return "capps" }
func (h *handler) Aliases() []string { return []string{"capp", "ca"} }

func (h *handler) RegisterGetCommand(parent *cobra.Command) {
	var allNamespaces bool
	cmd := &cobra.Command{
		Use:     "capps [name]",
		Aliases: []string{"capp", "ca"},
		Short:   "List Capps or get one by name",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := h.state.Cluster
			if cluster == "" {
				return fmt.Errorf("--cluster is required (or set it in your context)")
			}
			ns := h.state.Namespace

			if len(args) == 1 {
				if ns == "" {
					return fmt.Errorf("--namespace is required for get by name")
				}
				path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", cluster, ns, args[0])
				var item apitypes.CappResponse
				if err := h.state.Client.Get(cmd.Context(), path, &item); err != nil {
					return err
				}
				return h.render(cmd.OutOrStdout(), []apitypes.CappResponse{item}, item)
			}

			var list apitypes.CappListResponse
			if allNamespaces || ns == "" {
				path := fmt.Sprintf("/api/v1/clusters/%s/capps", cluster)
				if err := h.state.Client.Get(cmd.Context(), path, &list); err != nil {
					return err
				}
			} else {
				path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps", cluster, ns)
				if err := h.state.Client.Get(cmd.Context(), path, &list); err != nil {
					return err
				}
			}
			return h.render(cmd.OutOrStdout(), list.Items, list)
		},
	}
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "list across all namespaces")
	parent.AddCommand(cmd)
	cmd.ValidArgsFunction = h.completeCappNames
}

func (h *handler) RegisterCreateCommand(parent *cobra.Command) {
	var (
		name              string
		image             string
		metric            string
		cappState         string
		size              string
		minReplicas       int
		scaleDelaySeconds int
		containerName     string
		envPairs          []string
	)

	cmd := &cobra.Command{
		Use:     "capps",
		Aliases: []string{"capp", "ca"},
		Short:   "Create a Capp",
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := h.state.Cluster
			ns := h.state.Namespace
			if cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if ns == "" {
				return fmt.Errorf("--namespace is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if image == "" {
				return fmt.Errorf("--image is required")
			}

			envVars, err := parseEnvPairs(envPairs)
			if err != nil {
				return err
			}

			var cappSize apitypes.CappSize
			if size != "" {
				cappSize = apitypes.CappSize(size)
			}

			req := apitypes.CappRequest{
				Name:      name,
				Namespace: ns,
				Image:     image,
				Size:      cappSize,
				ScaleSpec: apitypes.ScaleSpec{
					Metric:            metric,
					MinReplicas:       minReplicas,
					ScaleDelaySeconds: scaleDelaySeconds,
				},
				State:         cappState,
				ContainerName: containerName,
				Env:           envVars,
			}

			path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps", cluster, ns)
			var created apitypes.CappResponse
			if err := h.state.Client.Post(cmd.Context(), path, req, &created); err != nil {
				return err
			}
			return h.render(cmd.OutOrStdout(), []apitypes.CappResponse{created}, created)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Capp name (required)")
	cmd.Flags().StringVar(&image, "image", "", "container image (required)")
	cmd.Flags().StringVar(&metric, "metric", "", "scale metric: concurrency|cpu|memory|rps")
	cmd.Flags().StringVar(&cappState, "state", "", "state: enabled|disabled")
	cmd.Flags().IntVar(&minReplicas, "min-replicas", 0, "minimum replica count")
	cmd.Flags().IntVar(&scaleDelaySeconds, "scale-delay-seconds", 0, "delay before scaling down to zero")
	cmd.Flags().StringVar(&containerName, "container-name", "", "container name")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "environment variable KEY=VALUE (repeatable)")
	cmd.Flags().StringVar(&size, "size", "", fmt.Sprintf("Capp size available values: %s, %s, %s", apitypes.CappSizeSmall, apitypes.CappSizeMedium, apitypes.CappSizeLarge)) // not required, server will default
	parent.AddCommand(cmd)
}

func (h *handler) RegisterUpdateCommand(parent *cobra.Command) {
	var (
		image             string
		metric            string
		cappState         string
		size              string
		minReplicas       int
		scaleDelaySeconds int
		containerName     string
		envPairs          []string
	)

	cmd := &cobra.Command{
		Use:     "capps <name>",
		Aliases: []string{"capp", "ca"},
		Short:   "Update a Capp (fetch current, apply flags, PUT)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := h.state.Cluster
			ns := h.state.Namespace
			if cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if ns == "" {
				return fmt.Errorf("--namespace is required")
			}
			cappName := args[0]

			getPath := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", cluster, ns, cappName)
			var current apitypes.CappResponse
			if err := h.state.Client.Get(cmd.Context(), getPath, &current); err != nil {
				return fmt.Errorf("fetching current state: %w", err)
			}

			// Seed request from current state to preserve all fields.
			req := apitypes.CappRequest{
				Name:          cappName,
				Namespace:     ns,
				Image:         current.Image,
				ScaleSpec:     current.ScaleSpec,
				State:         current.State,
				ContainerName: current.ContainerName,
				Size:          current.Size,
				Env:           current.Env,
				VolumeMounts:  current.VolumeMounts,
				RouteSpec:     current.RouteSpec,
				LogSpec:       current.LogSpec,
				NFSVolumes:    current.NFSVolumes,
			}

			if cmd.Flags().Changed("image") {
				req.Image = image
			}
			if cmd.Flags().Changed("metric") {
				req.ScaleSpec.Metric = metric
			}
			if cmd.Flags().Changed("state") {
				req.State = cappState
			}
			if cmd.Flags().Changed("min-replicas") {
				req.ScaleSpec.MinReplicas = minReplicas
			}
			if cmd.Flags().Changed("scale-delay-seconds") {
				req.ScaleSpec.ScaleDelaySeconds = scaleDelaySeconds
			}
			if cmd.Flags().Changed("container-name") {
				req.ContainerName = containerName
			}
			if cmd.Flags().Changed("env") {
				envVars, err := parseEnvPairs(envPairs)
				if err != nil {
					return err
				}
				req.Env = envVars
			}

			if cmd.Flags().Changed("size") {
				req.Size = apitypes.CappSize(size)
			}
			putPath := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", cluster, ns, cappName)
			var updated apitypes.CappResponse
			if err := h.state.Client.Put(cmd.Context(), putPath, req, &updated); err != nil {
				return err
			}
			return h.render(cmd.OutOrStdout(), []apitypes.CappResponse{updated}, updated)
		},
	}

	cmd.Flags().StringVar(&image, "image", "", "container image")
	cmd.Flags().StringVar(&metric, "metric", "", "scale metric: concurrency|cpu|memory|rps")
	cmd.Flags().StringVar(&cappState, "state", "", "state: enabled|disabled")
	cmd.Flags().IntVar(&minReplicas, "min-replicas", 0, "minimum replica count")
	cmd.Flags().IntVar(&scaleDelaySeconds, "scale-delay-seconds", 0, "delay before scaling down to zero")
	cmd.Flags().StringVar(&containerName, "container-name", "", "container name")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "environment variable KEY=VALUE (replaces all env vars)")
	cmd.Flags().StringVar(&size, "size", "", fmt.Sprintf("Capp size, available values: %s, %s, %s", apitypes.CappSizeSmall, apitypes.CappSizeMedium, apitypes.CappSizeLarge))
	parent.AddCommand(cmd)
	cmd.ValidArgsFunction = h.completeCappNames
}

func (h *handler) RegisterDeleteCommand(parent *cobra.Command) {
	var skipConfirm bool

	cmd := &cobra.Command{
		Use:     "capps <name>",
		Aliases: []string{"capp", "ca"},
		Short:   "Delete a Capp",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := h.state.Cluster
			ns := h.state.Namespace
			if cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if ns == "" {
				return fmt.Errorf("--namespace is required")
			}
			cappName := args[0]

			if !skipConfirm {
				fmt.Fprintf(cmd.OutOrStdout(), "Delete Capp %q in namespace %q on cluster %q? [y/N] ", cappName, ns, cluster) //nolint:errcheck
				var answer string
				fmt.Fscan(cmd.InOrStdin(), &answer) //nolint:errcheck
				if answer != "y" && answer != "Y" {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.") //nolint:errcheck
					return nil
				}
			}

			path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", cluster, ns, cappName)
			if err := h.state.Client.Delete(cmd.Context(), path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Capp %q deleted.\n", cappName) //nolint:errcheck
			return nil
		},
	}

	cmd.Flags().BoolVarP(&skipConfirm, "yes", "y", false, "skip confirmation prompt")
	parent.AddCommand(cmd)
	cmd.ValidArgsFunction = h.completeCappNames
}

// syncResult is the response returned by the sync endpoint.
type syncResult struct {
	CommitSHA string `json:"commitSha,omitempty"`
	Path      string `json:"path,omitempty"`
}

func (h *handler) RegisterSyncCommand(parent *cobra.Command) {
	cmd := &cobra.Command{
		Use:     "capps <name>",
		Aliases: []string{"capp", "ca"},
		Short:   "Sync a Capp to the GitOps repository",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := h.state.Cluster
			ns := h.state.Namespace
			if cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if ns == "" {
				return fmt.Errorf("--namespace is required")
			}
			cappName := args[0]

			path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s/sync", cluster, ns, cappName)
			var result syncResult
			if err := h.state.Client.Post(cmd.Context(), path, nil, &result); err != nil {
				return err
			}

			switch h.state.OutputFmt {
			case "json":
				return output.PrintJSON(cmd.OutOrStdout(), result)
			case "yaml":
				return output.PrintYAML(cmd.OutOrStdout(), result)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "Synced %q to git (commit: %s, path: %s)\n", cappName, result.CommitSHA, result.Path) //nolint:errcheck
			}
			return nil
		},
	}
	parent.AddCommand(cmd)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *handler) render(w io.Writer, items []apitypes.CappResponse, raw any) error {
	switch h.state.OutputFmt {
	case "json":
		return output.PrintJSON(w, raw)
	case "yaml":
		return output.PrintYAML(w, raw)
	case "wide":
		output.PrintTable(w, tableCols, items, true)
	default:
		output.PrintTable(w, tableCols, items, false)
	}
	return nil
}

// parseEnvPairs converts ["KEY=VALUE", ...] into []apitypes.EnvVar.
func parseEnvPairs(pairs []string) ([]apitypes.EnvVar, error) {
	result := make([]apitypes.EnvVar, 0, len(pairs))
	for _, p := range pairs {
		idx := 0
		for idx < len(p) && p[idx] != '=' {
			idx++
		}
		if idx == 0 {
			return nil, fmt.Errorf("invalid env var %q: key must not be empty", p)
		}
		if idx == len(p) {
			result = append(result, apitypes.EnvVar{Name: p})
		} else {
			result = append(result, apitypes.EnvVar{Name: p[:idx], Value: p[idx+1:]})
		}
	}
	return result, nil
}

// age converts an RFC3339 creation timestamp to a human-readable age string.
func age(createdAt string) string {
	if createdAt == "" {
		return "<unknown>"
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, createdAt)
	}
	if err != nil {
		return "<unknown>"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// completeCappNames fetches capp names from the API for shell completion.
// It reads the active context from config directly (PersistentPreRunE does not
// run during completion), builds a temporary client, and lists capps.
// Any error silently returns nothing — broken completion is better than an error.
func (h *handler) completeCappNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// already have a name, no more completions needed
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// load config the same way auth commands do
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	ctx, err := cfg.ActiveContext()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	if ctx.Server == "" || ctx.Token == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// resolve cluster and namespace: flag > state > context
	cluster := h.state.Cluster
	if cluster == "" {
		cluster = ctx.Cluster
	}

	ns := h.state.Namespace
	if ns == "" {
		ns = ctx.Namespace
	}

	if cluster == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	c := client.New(ctx.Server, ctx.Token, false)

	var list apitypes.CappListResponse
	var path string
	if ns == "" {
		path = fmt.Sprintf("/api/v1/clusters/%s/capps", cluster)
	} else {
		path = fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps", cluster, ns)
	}

	if err := c.Get(cmd.Context(), path, &list); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}

package root

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/resource"
)

// State holds resolved runtime values shared across all sub-commands.
// It is populated by PersistentPreRunE before any resource command runs.
// Auth and context commands annotate themselves with skipAuthAnnotation
// and load config directly.
type State struct {
	Cfg       *config.Config
	CfgPath   string
	ActiveCtx *config.Context
	Client    *client.Client
	OutputFmt string
	Cluster   string
	Namespace string
}

const skipAuthAnnotation = "cappctl/skip-auth"

// New builds the root cobra command, registers verb commands, and wires the registry.
// state must be a non-nil pointer; PersistentPreRunE will fill it before commands run.
func New(state *State, registry *resource.Registry) *cobra.Command {
	var (
		cfgPath   string
		ctxName   string
		server    string
		token     string
		cluster   string
		namespace string
		outputFmt string
		insecure  bool
	)

	root := &cobra.Command{
		Use:          "cappctl",
		Short:        "CLI for the CAPP backend API",
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			path := cfgPath
			if path == "" {
				path = config.DefaultPath()
			}
			state.CfgPath = path

			cfg, err := config.Load(path)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			state.Cfg = cfg

			// Auth/context commands only need config loaded, not a client.
			if cmd.Annotations[skipAuthAnnotation] == "true" {
				return nil
			}

			// Resolution order: flag > env var > active context.
			resolvedServer := firstNonEmpty(server, os.Getenv("CAPP_SERVER"))
			resolvedToken := firstNonEmpty(token, os.Getenv("CAPP_TOKEN"))
			resolvedCluster := firstNonEmpty(cluster, os.Getenv("CAPP_CLUSTER"))
			resolvedNamespace := firstNonEmpty(namespace, os.Getenv("CAPP_NAMESPACE"))

			if resolvedServer == "" || resolvedToken == "" {
				var activeCtx *config.Context
				if ctxName != "" {
					ctx, ok := cfg.GetContext(ctxName)
					if !ok {
						return fmt.Errorf("context %q not found", ctxName)
					}
					activeCtx = ctx
				} else {
					ctx, err := cfg.ActiveContext()
					if err != nil {
						return err
					}
					activeCtx = ctx
				}
				state.ActiveCtx = activeCtx

				if resolvedServer == "" {
					resolvedServer = activeCtx.Server
				}
				if resolvedCluster == "" {
					resolvedCluster = activeCtx.Cluster
				}
				if resolvedNamespace == "" {
					resolvedNamespace = activeCtx.Namespace
				}
				if resolvedToken == "" {
					resolvedToken, err = maybeRefresh(cmd, cfg, state.CfgPath, activeCtx, insecure)
					if err != nil {
						return err
					}
				}
			}

			if resolvedServer == "" {
				return fmt.Errorf("no server configured; use --server, set CAPP_SERVER, or run 'cappctl login'")
			}
			if resolvedToken == "" {
				return fmt.Errorf("no token available; run 'cappctl login' or pass --token / CAPP_TOKEN")
			}

			switch outputFmt {
			case "", "table", "wide", "json", "yaml":
				// valid
			default:
				return fmt.Errorf("invalid output format %q: must be one of table|wide|json|yaml", outputFmt)
			}

			state.Client = client.New(resolvedServer, resolvedToken, insecure)
			state.OutputFmt = outputFmt
			state.Cluster = resolvedCluster
			state.Namespace = resolvedNamespace
			return nil
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&cfgPath, "config", "", "config file (default: ~/.config/cappctl/config.yaml)")
	pf.StringVar(&ctxName, "context", "", "named context to use")
	pf.StringVar(&server, "server", "", "backend URL (env: CAPP_SERVER)")
	pf.StringVar(&token, "token", "", "bearer token (env: CAPP_TOKEN)")
	pf.StringVar(&cluster, "cluster", "", "target cluster (env: CAPP_CLUSTER)")
	pf.StringVar(&namespace, "namespace", "", "target namespace (env: CAPP_NAMESPACE)")
	pf.StringVarP(&outputFmt, "output", "o", "table", "output format: table|wide|json|yaml")
	pf.BoolVar(&insecure, "insecure", false, "skip TLS certificate verification")

	getCmd := &cobra.Command{Use: "get", Short: "Display one or many resources", Args: cobra.MinimumNArgs(1)}
	createCmd := &cobra.Command{Use: "create", Short: "Create a resource", Args: cobra.MinimumNArgs(1)}
	updateCmd := &cobra.Command{Use: "update", Short: "Update a resource", Args: cobra.MinimumNArgs(1)}
	deleteCmd := &cobra.Command{Use: "delete", Short: "Delete a resource", Args: cobra.MinimumNArgs(1)}

	registry.MountAll(getCmd, createCmd, updateCmd, deleteCmd)
	root.AddCommand(getCmd, createCmd, updateCmd, deleteCmd)

	return root
}

// SkipAuthAnnotation returns the annotation map that marks a command as not
// requiring API authentication (used by login/logout/context commands).
func SkipAuthAnnotation() map[string]string {
	return map[string]string{skipAuthAnnotation: "true"}
}

// firstNonEmpty returns the first non-empty string from vals.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// maybeRefresh returns the context token, auto-refreshing it if it expires
// within 30 seconds. On successful refresh, updates cfg and saves to disk.
func maybeRefresh(cmd *cobra.Command, cfg *config.Config, cfgPath string, ctx *config.Context, insecure bool) (string, error) {
	if ctx.Token == "" {
		return "", nil
	}
	if ctx.RefreshToken != "" && ctx.TokenExpiresAt.IsZero() {
		fmt.Fprintln(os.Stderr, "warning: refresh token set but token expiry unknown; skipping refresh") //nolint:errcheck
		return ctx.Token, nil
	}
	if ctx.RefreshToken == "" || ctx.TokenExpiresAt.IsZero() {
		return ctx.Token, nil
	}
	if time.Until(ctx.TokenExpiresAt) > 30*time.Second {
		return ctx.Token, nil
	}

	type refreshReq struct {
		RefreshToken string `json:"refreshToken"`
	}

	refreshClient := client.New(ctx.Server, "", insecure)
	var pair client.TokenPair
	if err := refreshClient.Post(cmd.Context(), "/api/v1/auth/refresh", refreshReq{RefreshToken: ctx.RefreshToken}, &pair); err != nil {
		return "", fmt.Errorf("refreshing token: %w", err)
	}

	ctx.Token = pair.AccessToken
	ctx.RefreshToken = pair.RefreshToken
	ctx.TokenExpiresAt = pair.ExpiresAt
	cfg.UpsertContext(*ctx)
	if err := config.Save(cfgPath, cfg); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not save refreshed token: %v\n", err) //nolint:errcheck
	}
	return pair.AccessToken, nil
}

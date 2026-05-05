package auth

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/root"
)

// NewContextCommand returns the `cappctl context` parent command with subcommands.
func NewContextCommand(state *root.State) *cobra.Command {
	parent := &cobra.Command{
		Use:         "context",
		Short:       "Manage named contexts",
		Annotations: root.SkipAuthAnnotation(),
	}

	parent.AddCommand(
		newContextListCommand(state),
		newContextUseCommand(state),
		newContextCurrentCommand(state),
	)
	return parent
}

func newContextListCommand(state *root.State) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List all contexts",
		Annotations: root.SkipAuthAnnotation(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(state.Cfg.Contexts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No contexts configured.") //nolint:errcheck
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			for _, ctx := range state.Cfg.Contexts {
				marker := "  "
				if ctx.Name == state.Cfg.CurrentContext {
					marker = "* "
				}
				fmt.Fprintf(tw, "%s%s\t%s\t%s\n", marker, ctx.Name, ctx.Server, ctx.AuthMode) //nolint:errcheck
			}
			tw.Flush() //nolint:errcheck
			return nil
		},
	}
}

func newContextUseCommand(state *root.State) *cobra.Command {
	return &cobra.Command{
		Use:         "use <name>",
		Short:       "Set the current context",
		Args:        cobra.ExactArgs(1),
		Annotations: root.SkipAuthAnnotation(),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if _, ok := state.Cfg.GetContext(name); !ok {
				return fmt.Errorf("context %q not found", name)
			}
			state.Cfg.CurrentContext = name
			if err := config.Save(state.CfgPath, state.Cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Switched to context %q.\n", name) //nolint:errcheck
			return nil
		},
	}
}

func newContextCurrentCommand(state *root.State) *cobra.Command {
	return &cobra.Command{
		Use:         "current",
		Short:       "Print the current context name",
		Annotations: root.SkipAuthAnnotation(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if state.Cfg.CurrentContext == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "(none)") //nolint:errcheck
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), state.Cfg.CurrentContext) //nolint:errcheck
			return nil
		},
	}
}

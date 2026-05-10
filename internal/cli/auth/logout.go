package auth

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/config"
	"github.com/dana-team/capp-backend/internal/cli/root"
)

// NewLogoutCommand returns the `cappctl logout` command.
func NewLogoutCommand(state *root.State) *cobra.Command {
	return &cobra.Command{
		Use:         "logout",
		Short:       "Clear stored credentials from a context",
		Annotations: root.SkipAuthAnnotation(),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --context is a root persistent flag; read the inherited value.
			name, _ := cmd.Flags().GetString("context")
			if name == "" {
				name = state.Cfg.CurrentContext
			}
			if name == "" {
				return fmt.Errorf("no context specified and no current context set")
			}

			ctx, ok := state.Cfg.GetContext(name)
			if !ok {
				return fmt.Errorf("context %q not found", name)
			}

			ctx.Token = ""
			ctx.RefreshToken = ""
			ctx.TokenExpiresAt = time.Time{}
			state.Cfg.UpsertContext(*ctx)

			if err := config.Save(state.CfgPath, state.Cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Credentials cleared from context %q.\n", name) //nolint:errcheck
			return nil
		},
	}
}

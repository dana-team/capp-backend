package auth

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dana-team/capp-backend/internal/cli/root"
)

// NewMCPHeadersCommand returns the `cappctl mcp-headers` command. It prints a
// JSON object of HTTP headers carrying a valid, auto-refreshed bearer token,
// in the shape Claude Code's MCP `headersHelper` expects. Wiring an MCP
// server's headersHelper to this command lets Claude Code re-run it on every
// connection and on 401/403, so a short-lived mcp token gets
// refreshed transparently without restarting the session.
func NewMCPHeadersCommand(state *root.State) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp-headers",
		Short: "Print Authorization headers for use as a Claude Code MCP headersHelper",
		RunE: func(cmd *cobra.Command, args []string) error {
			headers := map[string]string{
				"Authorization": "Bearer " + state.Token,
			}
			enc, err := json.Marshal(headers)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(enc)) //nolint:errcheck
			return nil
		},
	}
}

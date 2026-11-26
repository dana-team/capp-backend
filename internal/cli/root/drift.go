package root

import (
	"fmt"

	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/spf13/cobra"
)

// newDriftCommand returns the drift command and the revert and accept parents
// that resources attach to.
func newDriftCommand(state *State) (cmd, revert, accept *cobra.Command) {
	cmd = &cobra.Command{Use: "drift", Short: "Resolve differences between the cluster and the desired state", Args: cobra.MinimumNArgs(1)}
	revert = &cobra.Command{Use: "revert", Short: "Make the cluster match the desired state", Args: cobra.MinimumNArgs(1)}
	accept = &cobra.Command{Use: "accept", Short: "Make the desired state match the cluster", Args: cobra.MinimumNArgs(1)}
	adopt := &cobra.Command{
		Use:   "adopt",
		Short: "Record everything in the namespace that has no desired state yet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if state.Cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if state.Namespace == "" {
				return fmt.Errorf("--namespace is required")
			}
			var resp drift.AdoptResponse
			path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/drift/adopt", state.Cluster, state.Namespace)
			if err := state.Client.Post(cmd.Context(), path, nil, &resp); err != nil {
				return err
			}
			for _, item := range resp.Adopted {
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s\n", item.Kind, item.Name) //nolint:errcheck
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Adopted %d object(s).\n", resp.Total) //nolint:errcheck
			return nil
		},
	}
	cmd.AddCommand(revert, accept, adopt)
	return cmd, revert, accept
}

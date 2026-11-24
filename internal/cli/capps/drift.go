package capps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dana-team/capp-backend/internal/cli/client"
	apitypes "github.com/dana-team/capp-backend/pkg/api/capps"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/spf13/cobra"
)

const onDriftFlag = "on-drift"

func addOnDriftFlag(cmd *cobra.Command, v *string) {
	cmd.Flags().StringVar(v, onDriftFlag, "", "what to do when the Capp differs from its desired state: accept (keep the live changes) or revert (discard them); without it the command fails")
}

func validateOnDrift(onDrift string) error {
	switch onDrift {
	case "", drift.OnDriftAccept, drift.OnDriftRevert:
		return nil
	}
	return fmt.Errorf("--%s must be %s or %s", onDriftFlag, drift.OnDriftAccept, drift.OnDriftRevert)
}

// acceptQuery returns the query of a request that keeps live changes. A revert
// has no query: it is made before the request, see revertDrift.
func acceptQuery(onDrift string) string {
	if onDrift == drift.OnDriftAccept {
		return "?onDrift=" + drift.OnDriftAccept
	}
	return ""
}

// onDriftQuery returns the query of a request that sends no fields, so the
// backend can apply either choice on its own.
func onDriftQuery(onDrift string) string {
	if onDrift == "" {
		return ""
	}
	return "?onDrift=" + onDrift
}

// revertDrift reverts current when it has drifted and reads it again. An update
// is seeded from the live Capp, so it must not be read before the revert.
func (h *handler) revertDrift(ctx context.Context, path string, current *apitypes.CappResponse) error {
	if current.Drift == nil || current.Drift.State != drift.StateChanged {
		return nil
	}
	if err := h.postDrift(ctx, path, drift.OnDriftRevert, current.Drift); err != nil {
		return fmt.Errorf("reverting drift: %w", err)
	}
	*current = apitypes.CappResponse{}
	if err := h.state.Client.Get(ctx, path, current); err != nil {
		return fmt.Errorf("fetching reverted state: %w", err)
	}
	return nil
}

func (h *handler) postDrift(ctx context.Context, path, action string, seen *drift.Drift) error {
	request := drift.ActionRequest{Generation: seen.Generation, LiveVersion: seen.LiveVersion}
	return h.state.Client.Post(ctx, path+"/drift/"+action, request, nil)
}

// explainDrift adds the differences to the error of a refused update.
func explainDrift(err error) error {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	current := apiErr.Drift()
	if current == nil || current.State != drift.StateChanged {
		return err
	}
	return fmt.Errorf("the Capp differs from its desired state:\n%srepeat with --%s=%s to keep the live changes, or --%s=%s to discard them",
		describeDrift(current), onDriftFlag, drift.OnDriftAccept, onDriftFlag, drift.OnDriftRevert)
}

func describeDrift(d *drift.Drift) string {
	var b strings.Builder
	for _, field := range d.Fields {
		fmt.Fprintf(&b, "  %s: desired %s, live %s\n", field.Path, describeValue(field.Desired), describeValue(field.Live))
	}
	return b.String()
}

func describeValue(v any) string {
	if v == nil {
		return "(unset)"
	}
	return fmt.Sprintf("%v", v)
}

func driftState(c apitypes.CappResponse) string {
	if c.Drift == nil {
		return ""
	}
	return c.Drift.State
}

// RegisterDriftCommand attaches `drift revert capps` and `drift accept capps`.
func (h *handler) RegisterDriftCommand(revert, accept *cobra.Command) {
	h.registerDriftAction(revert, drift.OnDriftRevert, "Make the cluster match a Capp's desired state", "Overwrite the live Capp %q with its desired state?")
	h.registerDriftAction(accept, drift.OnDriftAccept, "Make a Capp's desired state match the cluster", "Replace the desired state of Capp %q with what is live?")
}

func (h *handler) registerDriftAction(parent *cobra.Command, action, short, question string) {
	var skipConfirm bool
	cmd := &cobra.Command{
		Use:     useWithName,
		Aliases: h.Aliases(),
		Short:   short,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if h.state.Cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			if h.state.Namespace == "" {
				return fmt.Errorf("--namespace is required")
			}
			path := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", h.state.Cluster, h.state.Namespace, args[0])
			current, err := h.readDrift(cmd.Context(), path)
			if err != nil {
				return err
			}
			if current.State == drift.StateInSync {
				fmt.Fprintf(cmd.OutOrStdout(), "Capp %q has not drifted.\n", args[0]) //nolint:errcheck
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Capp %q is %s.\n%s", args[0], current.State, describeDrift(current)) //nolint:errcheck
			if !skipConfirm && !confirmRollout(cmd, fmt.Sprintf(question, args[0])) {
				return nil
			}
			if err := h.postDrift(cmd.Context(), path, action, current); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Done.") //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().BoolVarP(&skipConfirm, "yes", "y", false, "skip confirmation prompt")
	parent.AddCommand(cmd)
	cmd.ValidArgsFunction = h.completeCappNames
}

// readDrift reads a Capp's drift, which the error carries when it is missing.
func (h *handler) readDrift(ctx context.Context, path string) (*drift.Drift, error) {
	var capp apitypes.CappResponse
	err := h.state.Client.Get(ctx, path, &capp)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Drift() != nil {
		return apiErr.Drift(), nil
	}
	if err != nil {
		return nil, err
	}
	if capp.Drift == nil {
		return nil, errors.New("the backend reports no drift for this Capp; desired state may be disabled")
	}
	if capp.Drift.State == drift.StateUnavailable {
		return nil, errors.New("the desired state store is unavailable")
	}
	return capp.Drift, nil
}

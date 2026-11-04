package capps

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apitypes "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
)

// RegisterRolloutCommand attaches the rollout sub-command to parent.
func (h *handler) RegisterRolloutCommand(parent *cobra.Command) {
	var (
		action         string
		revision       string
		removeStrategy bool
		skipConfirm    bool
	)

	cmd := &cobra.Command{
		Use:     useWithName,
		Aliases: h.Aliases(),
		Short:   "Promote, abort or retry a staged rollout, or remove the rollout strategy",
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
			if (action == "") == !removeStrategy {
				return fmt.Errorf("exactly one of --action or --remove-strategy is required")
			}
			cappName := args[0]
			cappPath := fmt.Sprintf("/api/v1/clusters/%s/namespaces/%s/capps/%s", cluster, ns, cappName)

			if removeStrategy {
				prompt := fmt.Sprintf("Remove the rollout strategy of Capp %q in namespace %q on cluster %q? This moves live traffic: a Ready candidate is promoted to 100%%, otherwise traffic is pinned to the stable revision.", cappName, ns, cluster)
				if !skipConfirm && !confirmRollout(cmd, prompt) {
					return nil
				}
				var updated apitypes.CappResponse
				if err := h.state.Client.Delete(cmd.Context(), cappPath+"/rollout/strategy", &updated); err != nil {
					return err
				}
				return h.render(cmd.OutOrStdout(), []apitypes.CappResponse{updated}, updated)
			}

			if action != "promote" && action != "abort" && action != "retry" {
				return fmt.Errorf("--action must be one of promote, abort or retry")
			}
			// Automation must name the revision it means: the candidate can change
			// between reading it and acting on it, with nobody there to see the prompt.
			if revision == "" && skipConfirm {
				return fmt.Errorf("--revision is required with -y")
			}
			if revision == "" {
				var current apitypes.CappResponse
				if err := h.state.Client.Get(cmd.Context(), cappPath, &current); err != nil {
					return err
				}
				if current.Status.Rollout == nil || current.Status.Rollout.CandidateRevision == "" {
					return fmt.Errorf("capp %q has no candidate revision", cappName)
				}
				revision = current.Status.Rollout.CandidateRevision
			}

			prompt := fmt.Sprintf("%s rollout of revision %q of Capp %q in namespace %q on cluster %q?", capitalize(action), revision, cappName, ns, cluster)
			if !skipConfirm && !confirmRollout(cmd, prompt) {
				return nil
			}

			body := apitypes.RolloutActionRequest{Action: action, Revision: revision}
			var updated apitypes.CappResponse
			if err := h.state.Client.Post(cmd.Context(), cappPath+"/rollout", body, &updated); err != nil {
				return err
			}
			// Stderr keeps -o json and -o yaml output parseable.
			fmt.Fprintf(cmd.ErrOrStderr(), "Action submitted; the operator applies it asynchronously. Check progress with: cappctl get capps %s -o yaml\n", cappName) //nolint:errcheck
			return h.render(cmd.OutOrStdout(), []apitypes.CappResponse{updated}, updated)
		},
	}

	cmd.Flags().StringVar(&action, "action", "", "rollout action: promote|abort|retry")
	cmd.Flags().StringVar(&revision, "revision", "", "candidate revision the action targets (default: the current candidate, required with -y)")
	cmd.Flags().BoolVar(&removeStrategy, "remove-strategy", false, "remove the rollout strategy (promotes a Ready candidate, otherwise pins traffic to stable)")
	cmd.Flags().BoolVarP(&skipConfirm, "yes", "y", false, "skip confirmation prompt")
	parent.AddCommand(cmd)
	cmd.ValidArgsFunction = h.completeCappNames
}

// confirmRollout asks for confirmation, reporting whether the user accepted.
func confirmRollout(cmd *cobra.Command, prompt string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt) //nolint:errcheck
	var answer string
	fmt.Fscan(cmd.InOrStdin(), &answer) //nolint:errcheck
	if answer != "y" && answer != "Y" {
		fmt.Fprintln(cmd.OutOrStdout(), "Aborted.") //nolint:errcheck
		return false
	}
	return true
}

func capitalize(s string) string {
	return strings.ToUpper(s[:1]) + s[1:]
}

// addRolloutFlags registers the rollout strategy flags shared by create and update.
func addRolloutFlags(cmd *cobra.Command, stages *[]string, autoRollback *bool, progressDeadline *string) {
	cmd.Flags().StringArrayVar(stages, "rollout-stage", nil, "rollout stage PERCENT[:DURATION], e.g. 10:5m (repeatable, replaces all stages)")
	cmd.Flags().BoolVar(autoRollback, "rollout-auto-rollback", false, "return traffic to the stable revision when the candidate fails")
	cmd.Flags().StringVar(progressDeadline, "rollout-progress-deadline", "", "how long the candidate may take to first become ready, e.g. 10m")
}

// applyRolloutFlags applies the rollout strategy flags onto req, keeping
// whatever strategy req already carries for flags that were not passed.
func applyRolloutFlags(cmd *cobra.Command, req *apitypes.CappRequest, stages []string, autoRollback bool, progressDeadline string) error {
	if !cmd.Flags().Changed("rollout-stage") && !cmd.Flags().Changed("rollout-auto-rollback") && !cmd.Flags().Changed("rollout-progress-deadline") {
		return nil
	}
	rs := apitypes.RouteSpec{}
	if req.RouteSpec != nil {
		rs = *req.RouteSpec
	}
	strategy := apitypes.RolloutStrategy{}
	if rs.RolloutStrategy != nil {
		strategy = *rs.RolloutStrategy
	}
	if cmd.Flags().Changed("rollout-stage") {
		parsed, err := parseRolloutStages(stages)
		if err != nil {
			return err
		}
		strategy.Stages = parsed
	}
	if cmd.Flags().Changed("rollout-auto-rollback") {
		strategy.AutoRollback = autoRollback
	}
	if cmd.Flags().Changed("rollout-progress-deadline") {
		strategy.ProgressDeadline = progressDeadline
	}
	if len(strategy.Stages) == 0 {
		return fmt.Errorf("--rollout-stage is required when the Capp has no rollout strategy")
	}
	rs.RolloutStrategy = &strategy
	req.RouteSpec = &rs
	return nil
}

// parseRolloutStages converts ["PERCENT[:DURATION]", ...] into rollout stages.
func parseRolloutStages(values []string) ([]apitypes.RolloutStage, error) {
	stages := make([]apitypes.RolloutStage, 0, len(values))
	for _, value := range values {
		percentStr, duration, hasDuration := strings.Cut(value, ":")
		percent, err := strconv.ParseInt(percentStr, 10, 64)
		if err != nil || percent < 0 || percent > 100 {
			return nil, fmt.Errorf("invalid --rollout-stage %q: percent must be an integer from 0 to 100", value)
		}
		if hasDuration {
			if _, err := time.ParseDuration(duration); err != nil {
				return nil, fmt.Errorf("invalid --rollout-stage %q: %w", value, err)
			}
		}
		stages = append(stages, apitypes.RolloutStage{Percent: &percent, Duration: duration})
	}
	return stages, nil
}

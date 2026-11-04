package capps

import (
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	rolloutActionPromote = "promote"
	rolloutActionAbort   = "abort"
	rolloutActionRetry   = "retry"
)

// rolloutAction handles POST /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/rollout
// It sets the rollout annotation, which the operator consumes asynchronously,
// so a success means the action was submitted rather than applied.
func (h *Handler) rolloutAction(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var req RolloutActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	if err := validateRolloutAction(&capp, req.Action, req.Revision); err != nil {
		apierrors.Respond(c, err)
		return
	}

	// The optimistic lock makes the checks above hold for the version being
	// written. Values files contain only the spec, so Git is not involved.
	patched := capp.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[k8s.AnnotationRollout] = req.Action + ":" + req.Revision
	patch := client.MergeFromWithOptions(&capp, client.MergeFromWithOptimisticLock{})
	if err := k8sClient.Patch(ctx, patched, patch); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusAccepted, FromK8s(patched, h.sizes))
}

// removeRolloutStrategy handles DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/rollout/strategy
// The operator then promotes a Ready candidate that is rolling out, and
// otherwise pins traffic to the stable revision.
func (h *Handler) removeRolloutStrategy(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var existing cappv1alpha1.Capp
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	if existing.Spec.RouteSpec.RolloutStrategy == nil {
		c.JSON(http.StatusOK, FromK8s(&existing, h.sizes))
		return
	}

	backup := h.isGitSyncEnabledForCapp(&existing)
	var meta cluster.ClusterMeta
	if backup {
		var err error
		meta, err = ExtractClusterMeta(c)
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}
	}

	updated := existing.DeepCopy()
	updated.Spec.RouteSpec.RolloutStrategy = nil
	if err := k8sClient.Update(ctx, updated); err != nil {
		apierrors.Respond(c, err)
		return
	}

	resp := FromK8s(updated, h.sizes)
	if backup {
		if _, err := h.syncCappValues(ctx, meta, updated); err != nil {
			resp.Warnings = append(resp.Warnings, BackupWarning(c, err))
		}
	}

	c.JSON(http.StatusOK, resp)
}

// validateRolloutAction rejects actions the Capp's status already shows the
// operator would refuse, because the operator reports a refusal only as a
// Warning event. The phase rules mirror isActive and retryable in the
// operator's internal/kinds/capp/rollout/rollout.go. Whether the candidate is
// Ready is not visible here, so the operator still decides that for promote.
func validateRolloutAction(capp *cappv1alpha1.Capp, action, revision string) error {
	if capp.Spec.RouteSpec.RolloutStrategy == nil {
		return apierrors.NewConflict(fmt.Sprintf("Capp %q has no rollout strategy", capp.Name))
	}
	status := capp.Status.Rollout
	if status == nil || status.CandidateRevision == "" {
		return apierrors.NewConflict(fmt.Sprintf("Capp %q has no candidate revision", capp.Name))
	}
	if pending, ok := capp.Annotations[k8s.AnnotationRollout]; ok {
		return apierrors.NewConflict(fmt.Sprintf("rollout action %q is still pending", pending))
	}
	if revision != status.CandidateRevision {
		return apierrors.NewConflict(fmt.Sprintf("revision %q is not the current candidate %q", revision, status.CandidateRevision))
	}

	var allowed bool
	switch action {
	case rolloutActionPromote, rolloutActionAbort:
		switch status.Phase {
		case cappv1alpha1.RolloutPhaseProgressing, cappv1alpha1.RolloutPhaseHolding,
			cappv1alpha1.RolloutPhasePromoting, cappv1alpha1.RolloutPhaseFailed:
			allowed = true
		}
	case rolloutActionRetry:
		switch status.Phase {
		case cappv1alpha1.RolloutPhaseFailed, cappv1alpha1.RolloutPhaseRolledBack, cappv1alpha1.RolloutPhaseAborted:
			allowed = true
		case cappv1alpha1.RolloutPhaseHolding:
			allowed = status.HeldPercent != nil
		}
	}
	if !allowed {
		return apierrors.NewConflict(fmt.Sprintf("cannot %s a rollout in phase %q", action, status.Phase))
	}
	return nil
}

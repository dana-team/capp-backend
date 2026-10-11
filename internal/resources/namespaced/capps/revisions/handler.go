// Package revisions exposes the rcs.dana.io/v1alpha1 CappRevision resource.
// The operator records a CappRevision every time a Capp's spec, labels or
// annotations change; this handler lists them and rolls a Capp back to one.
package revisions

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for CappRevisions. Revisions
// are addressed by their revision number under the owning Capp's path.
type Handler struct {
	gitopsEnabled bool
	gitops        capps.GitOpsSyncer
	sizes         config.CappSizes
}

// New returns a ready-to-use CappRevision Handler. When gitops is disabled,
// pass nil for the syncer — rollbacks then skip the Git backup.
func New(gitopsEnabled bool, gitops capps.GitOpsSyncer, sizes config.CappSizes) *Handler {
	return &Handler{gitopsEnabled: gitopsEnabled, gitops: gitops, sizes: sizes}
}

// Name returns the handler's identifier, matching the resources.capprevisions config key.
func (h *Handler) Name() string { return "capprevisions" }

// RegisterRoutes attaches all CappRevision routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rev := rg.Group("/namespaces/:namespace/capps/:name/revisions")
	rev.GET("", h.list)
	rev.GET("/:revision", h.get)
	rev.POST("/:revision/rollback", h.rollback)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// list handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions
// Revisions are returned newest first.
func (h *Handler) list(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	revisions, err := listRevisions(c, k8sClient, c.Param("namespace"), c.Param("name"))
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	items := make([]capps.CappRevisionResponse, 0, len(revisions))
	for i := range revisions {
		items = append(items, h.fromK8s(&revisions[i]))
	}
	c.JSON(http.StatusOK, capps.CappRevisionListResponse{Items: items, Total: len(items)})
}

// get handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions/:revision
func (h *Handler) get(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	revision, err := findRevision(c, k8sClient)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, h.fromK8s(revision))
}

// rollback handles POST /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions/:revision/rollback
// It replaces the live Capp's spec, labels and annotations with the revision's
// template. The operator then records the result as a new revision.
func (h *Handler) rollback(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	revision, err := findRevision(c, k8sClient)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	var existing cappv1alpha1.Capp
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	updated := applyTemplate(&existing, revision.Spec.CappTemplate)

	backup := h.gitopsEnabled && h.gitops != nil && k8s.HasBackupLabel(existing.Labels)
	var meta cluster.ClusterMeta
	if backup {
		meta, err = capps.ExtractClusterMeta(c)
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}
	}

	if err := k8sClient.Update(ctx, updated); err != nil {
		apierrors.Respond(c, err)
		return
	}

	resp := capps.FromK8s(updated, h.sizes)
	if backup {
		if _, err := capps.SyncCappValues(ctx, h.gitops, meta, updated); err != nil {
			resp.Warnings = append(resp.Warnings, capps.BackupWarning(c, err))
		}
	}

	c.JSON(http.StatusOK, resp)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// listRevisions returns the Capp's revisions sorted by revision number, newest first.
func listRevisions(c *gin.Context, k8sClient client.Client, namespace, cappName string) ([]cappv1alpha1.CappRevision, error) {
	var list cappv1alpha1.CappRevisionList
	if err := k8sClient.List(c.Request.Context(), &list,
		client.InNamespace(namespace),
		client.MatchingLabels{k8s.LabelCappName: cappName},
	); err != nil {
		return nil, err
	}
	slices.SortFunc(list.Items, func(a, b cappv1alpha1.CappRevision) int {
		return b.Spec.RevisionNumber - a.Spec.RevisionNumber
	})
	return list.Items, nil
}

// findRevision resolves the :revision URL param to a CappRevision. Revisions
// are matched on spec.revisionNumber rather than object name, since the
// operator hashes the name when the Capp name is long.
func findRevision(c *gin.Context, k8sClient client.Client) (*cappv1alpha1.CappRevision, error) {
	param := c.Param("revision")
	number, err := strconv.Atoi(param)
	if err != nil || number < 1 {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("invalid revision %q: must be a positive integer", param))
	}

	revisions, err := listRevisions(c, k8sClient, c.Param("namespace"), c.Param("name"))
	if err != nil {
		return nil, err
	}
	for i := range revisions {
		if revisions[i].Spec.RevisionNumber == number {
			return &revisions[i], nil
		}
	}
	return nil, apierrors.NewNotFound("CappRevision", fmt.Sprintf("%s revision %d", c.Param("name"), number))
}

// applyTemplate returns a copy of live with the template's spec, labels and
// annotations. The backup-to-git label is kept as it is on the live Capp:
// it tracks the current state of the GitOps repo, not of the Capp spec.
func applyTemplate(live *cappv1alpha1.Capp, tmpl cappv1alpha1.CappTemplate) *cappv1alpha1.Capp {
	updated := live.DeepCopy()
	updated.Spec = *tmpl.Spec.DeepCopy()
	updated.Labels = maps.Clone(tmpl.Labels)
	updated.Annotations = maps.Clone(tmpl.Annotations)

	delete(updated.Labels, k8s.LabelBackupToGit)
	if v, ok := live.Labels[k8s.LabelBackupToGit]; ok {
		if updated.Labels == nil {
			updated.Labels = map[string]string{}
		}
		updated.Labels[k8s.LabelBackupToGit] = v
	}
	return updated
}

// fromK8s converts a CappRevision into its response DTO. Name, namespace and
// metadata identify the revision object; labels and annotations are the
// Capp's as recorded in the template.
func (h *Handler) fromK8s(rev *cappv1alpha1.CappRevision) capps.CappRevisionResponse {
	meta := *rev.ObjectMeta.DeepCopy()
	meta.Labels = rev.Spec.CappTemplate.Labels
	meta.Annotations = rev.Spec.CappTemplate.Annotations
	return capps.CappRevisionResponse{
		BareCappResponse: capps.BareFromK8s(meta, rev.Spec.CappTemplate.Spec, h.sizes),
		RevisionNumber:   rev.Spec.RevisionNumber,
	}
}

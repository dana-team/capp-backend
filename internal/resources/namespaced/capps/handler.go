package capps

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GitOpsSyncer is the subset of the gitops.Client interface that the
// sync handler needs. Using an interface allows unit testing with fakes.
type GitOpsSyncer interface {
	SyncValues(ctx context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte) (string, error)
	DeleteValues(ctx context.Context, gitOpsPath, namespace, cappName string) (string, error)
	BuildRelPath(gitOpsPath, namespace, cappName string) string
}

// Handler implements resources.ResourceHandler for the rcs.dana.io/v1alpha1
// Capp custom resource. It provides full CRUD over Capps through the
// per-request scoped Kubernetes client injected by the cluster middleware.
type Handler struct {
	gitopsEnabled bool
	gitops        GitOpsSyncer
	sizes         config.CappSizes
}

// New returns a ready-to-use Capp Handler. When gitops is disabled, pass nil
// for the syncer — the sync endpoint will return 501.
func New(gitopsEnabled bool, gitops GitOpsSyncer, sizes config.CappSizes) *Handler {
	return &Handler{gitopsEnabled: gitopsEnabled, gitops: gitops, sizes: sizes}
}

// Name returns the handler's identifier, matching the resources.capps config key.
func (h *Handler) Name() string { return "capps" }

// RegisterRoutes attaches all Capp routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// Cluster-scoped list (all namespaces).
	rg.GET("/capps", h.listAll)

	// Namespace-scoped operations.
	ns := rg.Group("/namespaces/:namespace/capps")
	ns.GET("", h.list)
	ns.POST("", h.create)
	ns.GET("/:name", h.get)
	ns.PUT("/:name", h.update)
	ns.DELETE("/:name", h.delete)
	ns.POST("/:name/sync", h.sync)
	ns.DELETE("/:name/sync", h.unsync)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// listAll handles GET /api/v1/clusters/:cluster/capps
// Lists Capps across all namespaces visible to the caller.
func (h *Handler) listAll(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	var cappList cappv1alpha1.CappList
	if err := k8sClient.List(c.Request.Context(), &cappList); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.respondList(c, cappList)
}

// list handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps
func (h *Handler) list(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	var cappList cappv1alpha1.CappList
	if err := k8sClient.List(c.Request.Context(), &cappList, client.InNamespace(namespace)); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.respondList(c, cappList)
}

// get handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
func (h *Handler) get(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, FromK8s(&capp, h.sizes))
}

// create handles POST /api/v1/clusters/:cluster/namespaces/:namespace/capps
func (h *Handler) create(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	var req CappRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	capp, err := ToK8s(req, nil, namespace, h.sizes)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}
	if err := k8sClient.Create(c.Request.Context(), capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusCreated, FromK8s(capp, h.sizes))
}

// update handles PUT /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
// It performs a full replacement (not a patch). The resource version is read
// from the live object before writing so we don't clobber concurrent changes.
func (h *Handler) update(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var req CappRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	// Read the current live object to get its resourceVersion. This is
	// required by Kubernetes optimistic concurrency control.
	var existing cappv1alpha1.Capp
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	updated, err := ToK8s(req, &existing, namespace, h.sizes)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}
	updated.Name = name // URL param is authoritative

	if err := k8sClient.Update(ctx, updated); err != nil {
		apierrors.Respond(c, err)
		return
	}

	if h.isGitSyncEnabledForCapp(&existing) {
		meta, err := extractClusterMeta(c)
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}
		if _, err := h.syncCappValues(ctx, meta, updated); err != nil {
			apierrors.Respond(c, apierrors.NewGitOpsSyncFailed(err))
			return
		}
	}

	c.JSON(http.StatusOK, FromK8s(updated, h.sizes))
}

// delete handles DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
func (h *Handler) delete(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	if err := k8sClient.Delete(ctx, &capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	if h.isGitSyncEnabledForCapp(&capp) {
		meta, err := extractClusterMeta(c)
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}
		if _, err := h.gitops.DeleteValues(ctx, meta.Name, namespace, name); err != nil {
			apierrors.Respond(c, apierrors.NewGitOpsSyncFailed(err))
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// SyncResponse is the JSON body returned by the sync endpoints. Exported so
// non-Gin clients (e.g. cappctl, the MCP server) can decode it directly
// instead of duplicating the shape.
type SyncResponse struct {
	Enabled   bool   `json:"enabled"`
	CommitSHA string `json:"commitSha,omitempty"`
	Path      string `json:"path,omitempty"`
}

// sync handles POST /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync
// It enables Git sync (adds the backup label) and pushes the current values.
func (h *Handler) sync(c *gin.Context) {
	h.setGitSync(c, true)
}

// unsync handles DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync
// It disables Git sync: removes the values file and the backup label from the capp.
func (h *Handler) unsync(c *gin.Context) {
	h.setGitSync(c, false)
}

func (h *Handler) setGitSync(c *gin.Context, enable bool) {
	if !h.isGitOpsConfigured() {
		apierrors.Respond(c, apierrors.NewNotSupported("sync"))
		return
	}

	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	meta, err := extractClusterMeta(c)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}

	patched := capp.DeepCopy()
	if enable {
		if patched.Labels == nil {
			patched.Labels = map[string]string{}
		}
		patched.Labels[k8s.LabelBackupToGit] = "true"
	} else {
		delete(patched.Labels, k8s.LabelBackupToGit)
	}

	if err := k8sClient.Patch(ctx, patched, client.MergeFrom(&capp)); err != nil {
		apierrors.Respond(c, err)
		return
	}

	var commitSHA string
	if enable {
		commitSHA, err = h.syncCappValues(ctx, meta, &capp)
	} else {
		commitSHA, err = h.gitops.DeleteValues(ctx, meta.Name, namespace, name)
	}
	if err != nil {
		apierrors.Respond(c, apierrors.NewGitOpsSyncFailed(err))
		return
	}

	c.JSON(http.StatusOK, SyncResponse{
		Enabled:   enable,
		CommitSHA: commitSHA,
		Path:      h.gitops.BuildRelPath(meta.Name, namespace, name),
	})
}

// Git sync helpers

func (h *Handler) isGitOpsConfigured() bool {
	return h.gitopsEnabled && h.gitops != nil
}

func (h *Handler) isGitSyncEnabledForCapp(capp *cappv1alpha1.Capp) bool {
	return h.isGitOpsConfigured() && k8s.HasBackupLabel(capp.Labels)
}

func (h *Handler) syncCappValues(ctx context.Context, meta cluster.ClusterMeta, capp *cappv1alpha1.Capp) (string, error) {
	valuesYAML, err := GenerateValues(capp)
	if err != nil {
		return "", fmt.Errorf("generate values: %w", err)
	}
	return h.gitops.SyncValues(ctx, meta.Name, capp.Namespace, capp.Name, valuesYAML)
}

// extractClusterMeta retrieves the ClusterMeta from the Gin context.
func extractClusterMeta(c *gin.Context) (cluster.ClusterMeta, error) {
	val, exists := c.Get(string(middleware.ClusterMetaKey))
	if !exists {
		return cluster.ClusterMeta{}, fmt.Errorf("ClusterMeta not found in context")
	}
	meta, ok := val.(cluster.ClusterMeta)
	if !ok {
		return cluster.ClusterMeta{}, fmt.Errorf("ClusterMeta has unexpected type in context")
	}
	return meta, nil
}

// respondList converts a CappList into a CappListResponse and writes it.
func (h *Handler) respondList(c *gin.Context, cappList cappv1alpha1.CappList) {
	items := make([]CappResponse, 0, len(cappList.Items))
	for i := range cappList.Items {
		items = append(items, FromK8s(&cappList.Items[i], h.sizes))
	}
	c.JSON(http.StatusOK, CappListResponse{Items: items, Total: len(items)})
}

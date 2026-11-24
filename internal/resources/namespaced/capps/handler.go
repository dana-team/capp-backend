package capps

import (
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for the rcs.dana.io/v1alpha1
// Capp custom resource. It provides full CRUD over Capps through the
// per-request scoped Kubernetes client prepared by the active authorization middleware.
type Handler struct {
	clusterMgr    cluster.ClusterManager
	sizes         config.CappSizes
	observability config.ObservabilityConfig
	prometheus    promv1.API
	tracker       *tracking.Tracker
	// logStreams holds one token per open log stream; its capacity is
	// resources.capps.maxLogStreams.
	logStreams chan struct{}
}

// New returns a ready-to-use Capp Handler. Pass nil for clusterMgr when
// migration is not needed (e.g. in tests that don't exercise migrate).
// When observability is disabled, the metrics endpoints return 501.
//
// It returns an error only when observability is enabled and the Prometheus
// client cannot be built; config.Validate already parses QueryURL, so this
// is a startup-time configuration failure, not a condition the metrics
// endpoints should have to handle at request time.
func New(clusterMgr cluster.ClusterManager, sizes config.CappSizes, observability config.ObservabilityConfig) (*Handler, error) {
	h := &Handler{clusterMgr: clusterMgr, sizes: sizes, observability: observability,
		logStreams: make(chan struct{}, defaultMaxLogStreams)}
	prometheus, err := newPrometheusAPI(observability)
	if err != nil {
		return nil, fmt.Errorf("building prometheus client: %w", err)
	}
	h.prometheus = prometheus
	return h, nil
}

// WithMaxLogStreams sets how many log streams the handler serves at a time
// (resources.capps.maxLogStreams). A value below 1 keeps the default.
func (h *Handler) WithMaxLogStreams(n int) *Handler {
	if n >= 1 {
		h.logStreams = make(chan struct{}, n)
	}
	return h
}

// WithTracker records the desired state of Capps and reports their drift. A nil
// tracker leaves both off.
func (h *Handler) WithTracker(tracker *tracking.Tracker) *Handler {
	h.tracker = tracker
	return h
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
	ns.POST("/:name/migrate", h.migrate)
	ns.POST("/:name/rollout", h.rolloutAction)
	ns.DELETE("/:name/rollout/strategy", h.removeRolloutStrategy)
	ns.GET("/:name/metrics", h.listMetrics)
	ns.GET("/:name/metrics/:metric", h.queryMetric)
	ns.GET("/:name/logs", h.logs)

	tracking.Actions{
		Tracker: h.tracker,
		Kind:    desiredstate.KindCapp,
		New:     func() client.Object { return &cappv1alpha1.Capp{} },
	}.Register(ns)
}

func cappRef(c *gin.Context) desiredstate.Ref {
	return tracking.Ref(c, desiredstate.KindCapp, c.Param("namespace"), c.Param("name"))
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// listAll handles GET /api/v1/clusters/:cluster/capps
// Lists Capps across all namespaces visible to the caller.
func (h *Handler) listAll(c *gin.Context) {
	var cappList cappv1alpha1.CappList
	if !namespaced.ListAll(c, &cappList) {
		return
	}

	drifts, missing := h.tracker.InspectAll(c, desiredstate.KindCapp, tracking.Objects(cappList.Items))
	h.respondList(c, cappList, drifts, missing)
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

	drifts, missing := h.tracker.InspectList(c, desiredstate.KindCapp, namespace, tracking.Objects(cappList.Items))
	h.respondList(c, cappList, drifts, missing)
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
			err = apierrors.NewNotFound("Capp", name)
		}
		h.tracker.RespondReadError(c, cappRef(c), nil, err)
		return
	}

	resp := FromK8s(&capp, h.sizes)
	resp.Drift = h.tracker.Inspect(c, cappRef(c), &capp)
	c.JSON(http.StatusOK, resp)
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
	write := h.tracker.Begin(c, tracking.Ref(c, desiredstate.KindCapp, namespace, capp.Name))
	if err := k8sClient.Create(c.Request.Context(), capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	resp := FromK8s(capp, h.sizes)
	resp.Warnings = append(resp.Warnings, write.Save(c, k8sClient, capp)...)
	c.JSON(http.StatusCreated, resp)
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

	write := h.tracker.Begin(c, cappRef(c))
	if _, ok := write.ResolveDrift(c, &existing); !ok {
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

	resp := FromK8s(updated, h.sizes)
	resp.Warnings = append(resp.Warnings, write.Save(c, k8sClient, updated)...)
	c.JSON(http.StatusOK, resp)
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

	write := h.tracker.Begin(c, cappRef(c))
	if err := k8sClient.Delete(ctx, &capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	tracking.RespondDeleted(c, write.Delete(c))
}

// migrate handles POST /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate
func (h *Handler) migrate(c *gin.Context) {
	var sourceClient client.Client
	if !policy.Enabled(c) {
		sourceClient = namespaced.ExtractClient(c)
		if sourceClient == nil {
			return
		}
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	var req MigrateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	meta, err := ExtractClusterMeta(c)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}

	if req.TargetCluster == meta.Name && req.TargetNamespace == namespace {
		apierrors.Respond(c, apierrors.NewBadRequest("cannot migrate to the same cluster and namespace"))
		return
	}

	var targetClient client.Client
	if policy.Enabled(c) {
		var ok bool
		sourceClient, targetClient, ok = policy.PrepareMigration(c, req.TargetCluster, req.TargetNamespace)
		if !ok {
			return
		}
	} else {
		targetClient = h.kubernetesMigrationTarget(c, req)
		if targetClient == nil {
			return
		}
	}
	ctx := c.Request.Context()

	var sourceCapp cappv1alpha1.Capp
	if err := sourceClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sourceCapp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	var existingTarget cappv1alpha1.Capp
	if err := targetClient.Get(ctx, client.ObjectKey{Namespace: req.TargetNamespace, Name: name}, &existingTarget); err == nil {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf("Capp %q already exists", name)))
		return
	} else if !k8serrors.IsNotFound(err) {
		apierrors.Respond(c, err)
		return
	}

	sourceHostname := sourceCapp.Spec.RouteSpec.Hostname
	effectiveHostname, err := resolveTargetHostname(sourceHostname, req.TargetHostname, req.DeleteSource)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	secrets, configMaps, err := listManagedResources(ctx, sourceClient, namespace)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("list managed resources: %w", err)))
		return
	}

	if err := copyDependentResources(ctx, targetClient, req.TargetNamespace, secrets, configMaps); err != nil {
		apierrors.Respond(c, err)
		return
	}

	targetCapp := prepareCapp(&sourceCapp, req.TargetNamespace, effectiveHostname)
	if err := targetClient.Create(ctx, targetCapp); err != nil {
		if cleanupErr := cleanupResources(ctx, targetClient, req.TargetNamespace, secrets, configMaps); cleanupErr != nil {
			_ = c.Error(fmt.Errorf("rollback copied resources: %w", cleanupErr))
		}
		apierrors.Respond(c, err)
		return
	}

	resp := MigrateResponse{
		Name:            name,
		SourceCluster:   meta.Name,
		SourceNamespace: namespace,
		TargetCluster:   req.TargetCluster,
		TargetNamespace: req.TargetNamespace,
	}

	for i := range secrets {
		resp.CopiedSecrets = append(resp.CopiedSecrets, secrets[i].Name)
	}
	for i := range configMaps {
		resp.CopiedConfigMaps = append(resp.CopiedConfigMaps, configMaps[i].Name)
	}
	resp.Warnings = h.recordMigrated(c, targetClient, req, targetCapp, secrets, configMaps)

	if req.DeleteSource {
		if err := sourceClient.Delete(ctx, &sourceCapp); err != nil {
			resp.SourceDeleted = false
			c.JSON(http.StatusOK, resp)
			return
		}
		resp.SourceDeleted = true
		resp.Warnings = append(resp.Warnings, h.tracker.Forget(c, cappRef(c))...)

		if _, hasBypass := targetCapp.Annotations[migrationBypassAnnotation]; hasBypass {
			patch := client.MergeFrom(targetCapp.DeepCopy())
			delete(targetCapp.Annotations, migrationBypassAnnotation)
			if err := targetClient.Patch(ctx, targetCapp, patch); err != nil {
				_ = c.Error(fmt.Errorf("remove bypass annotation: %w", err))
			}
		}
	}

	c.JSON(http.StatusOK, resp)
}

// ExtractClusterMeta retrieves the ClusterMeta from the Gin context.
func ExtractClusterMeta(c *gin.Context) (cluster.ClusterMeta, error) {
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
func (h *Handler) respondList(c *gin.Context, cappList cappv1alpha1.CappList, drifts []*drift.Drift, missing []drift.Missing) {
	items := make([]CappResponse, 0, len(cappList.Items))
	for i := range cappList.Items {
		item := FromK8s(&cappList.Items[i], h.sizes)
		item.Drift = drifts[i]
		items = append(items, item)
	}
	c.JSON(http.StatusOK, CappListResponse{Items: items, Total: len(items), Missing: missing})
}

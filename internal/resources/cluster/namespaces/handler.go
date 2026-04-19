// Package namespaces implements the namespace listing resource handler.
//
// It exposes a single endpoint:
//
//	GET /api/v1/clusters/:cluster/namespaces
//
// The frontend uses this endpoint to populate the namespace dropdown when
// the user selects a cluster. The response is a simplified list — not the
// full Kubernetes Namespace object — so the frontend does not need to
// understand Kubernetes API conventions.
//
// On OpenShift clusters, it lists project.openshift.io/v1 Projects using the
// user-scoped client — the Projects API automatically returns only the projects
// the user has access to. On vanilla Kubernetes, it uses the admin client to list
// all CAPP-managed namespaces and then filters them with a SelfSubjectAccessReview
// for each namespace to return only those the user can create Capps in.
package namespaces

import (
	"context"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/utils"
	"github.com/gin-gonic/gin"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NamespaceItem is the simplified namespace representation returned to the frontend.
type NamespaceItem struct {
	// Name is the Kubernetes namespace name.
	Name string `json:"name"`

	// Status is the namespace phase: "Active" or "Terminating".
	Status string `json:"status"`
}

// NamespaceListResponse is the response envelope for the list endpoint.
type NamespaceListResponse struct {
	Items []NamespaceItem `json:"items"`
}

// Handler implements resources.ResourceHandler for Kubernetes Namespaces.
type Handler struct{}

// New returns a ready-to-use namespace Handler.
func New() *Handler { return &Handler{} }

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "namespaces" }

// RegisterRoutes attaches the namespace routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/namespaces", h.list)
	rg.POST("/namespaces", h.create)
}

// list handles GET /api/v1/clusters/:cluster/namespaces.
//
// On OpenShift the Projects API is used with the user-scoped client, which
// returns only projects the user has access to. On vanilla Kubernetes the admin
// client lists all CAPP-labeled namespaces and a SelfSubjectAccessReview filters
// to those where the user can create Capps.
func (h *Handler) list(c *gin.Context) {
	userClient, ok := c.MustGet(string(middleware.K8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("K8sClientKey")))
		return
	}

	adminClient, ok := c.MustGet(string(middleware.AdminK8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("AdminK8sClientKey")))
		return
	}

	meta, ok := c.MustGet(string(middleware.ClusterMetaKey)).(cluster.ClusterMeta)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("ClusterMetaKey")))
		return
	}

	labelSelector := labels.SelectorFromSet(labels.Set{consts.ManagedNameSpaceLabelKey: "true"})
	listOpts := &client.ListOptions{LabelSelector: labelSelector}

	var items []NamespaceItem

	if meta.IsOpenShift {
		// On OpenShift, the Projects API automatically returns only the projects
		// the user has access to — no per-namespace SAR filtering needed.
		projectList := &unstructured.UnstructuredList{}
		projectList.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "project.openshift.io", Version: "v1", Kind: "ProjectList",
		})
		if err := userClient.List(c.Request.Context(), projectList, listOpts); err != nil {
			apierrors.Respond(c, err)
			return
		}
		items = make([]NamespaceItem, 0, len(projectList.Items))
		for _, p := range projectList.Items {
			phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
			items = append(items, NamespaceItem{Name: p.GetName(), Status: phase})
		}
	} else {
		// On vanilla Kubernetes: admin client lists all CAPP-managed namespaces,
		// then filter to only those the user can create Capps in.
		var nsList corev1.NamespaceList
		if err := adminClient.List(c.Request.Context(), &nsList, listOpts); err != nil {
			apierrors.Respond(c, err)
			return
		}
		items = make([]NamespaceItem, 0, len(nsList.Items))
		for _, ns := range nsList.Items {
			allowed, err := canCreateCapps(c.Request.Context(), userClient, ns.Name)
			if err != nil || !allowed {
				continue
			}
			items = append(items, NamespaceItem{Name: ns.Name, Status: string(ns.Status.Phase)})
		}
	}

	c.JSON(http.StatusOK, NamespaceListResponse{Items: items})
}

// canCreateCapps performs a SelfSubjectAccessReview to check whether the current
// user (as represented by userClient's credentials) can create Capps in namespace.
func canCreateCapps(ctx context.Context, userClient client.Client, namespace string) (bool, error) {
	sar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "create",
				Group:     consts.CappAPIGroup,
				Resource:  consts.CappResource,
			},
		},
	}
	if err := userClient.Create(ctx, sar); err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

// create handles POST /api/v1/clusters/:cluster/namespaces.

func (h *Handler) create(c *gin.Context) {
	userClient, ok := c.MustGet(string(middleware.K8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("K8sClientKey")))
		return
	}

	allowed, err := canCreateNamespaces(c.Request.Context(), userClient)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	if !allowed {
		apierrors.Respond(c, apierrors.NewForbidden("not allowed to create namespaces"))
		return
	}
	adminClient, ok := c.MustGet(string(middleware.AdminK8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("AdminK8sClientKey")))
		return
	}

	var body struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	ns := &corev1.Namespace{}
	ns.Name = body.Name
	ns.Labels = map[string]string{
		consts.ManagedNameSpaceLabelKey: "true",
	}

	if err := adminClient.Create(c.Request.Context(), ns); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusCreated, NamespaceItem{Name: ns.Name, Status: "Active"})
}

func canCreateNamespaces(ctx context.Context, userClient client.Client) (bool, error) {
	sar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     "create",
				Group:    "",
				Resource: "namespaces",
			},
		},
	}
	if err := userClient.Create(ctx, sar); err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

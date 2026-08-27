// Package namespaces implements namespace resource handlers.
//
// Endpoints:
//
//	GET    /api/v1/clusters/:cluster/namespaces          — list CAPP-managed namespaces
//	GET	   /api/v1/clusters/:cluster/namespaces/:namespace    — get namespace details
//	POST   /api/v1/clusters/:cluster/namespaces          — create namespace with quota + RoleBinding
//	PUT    /api/v1/clusters/:cluster/namespaces/:namespace    — replace quota and RoleBinding
//	PATCH  /api/v1/clusters/:cluster/namespaces/:namespace    — add users to existing RoleBinding
//
// On OpenShift clusters, it lists project.openshift.io/v1 Projects using the
// user-scoped client — the Projects API automatically returns only the projects
// the user has access to. On vanilla Kubernetes, it uses the admin client to list
// all CAPP-managed namespaces and then filters them with a SelfSubjectAccessReview
// for each namespace to return only those the user can create Capps in.
package namespaces

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/utils"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	clusterRoleName = "capp-user"
	clusterRoleKind = "ClusterRole"
)

// Handler implements resources.ResourceHandler for Kubernetes Namespaces.
type Handler struct {
	logger *zap.Logger
}

// New returns a ready-to-use namespace Handler.
func New(logger *zap.Logger) *Handler { return &Handler{logger: logger} }

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "namespaces" }

// RegisterRoutes attaches the namespace routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/namespaces", h.list)
	rg.GET("/namespaces/:namespace", h.get)
	rg.POST("/namespaces", h.create)
	rg.PUT("/namespaces/:namespace", h.update)
	rg.PATCH("/namespaces/:namespace", h.patch)
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

	quotaByNS := listQuotaInfoMap(c.Request.Context(), adminClient, h.logger)

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
			item := NamespaceItem{Name: p.GetName(), Status: phase}
			item.Quota = quotaByNS[p.GetName()]
			items = append(items, item)
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
			item := NamespaceItem{Name: ns.Name, Status: string(ns.Status.Phase)}
			item.Quota = quotaByNS[ns.Name]
			items = append(items, item)
		}
	}

	canCreate, _ := canCreateNamespaces(c.Request.Context(), userClient)

	c.JSON(http.StatusOK, NamespaceListResponse{Items: items, CanCreate: canCreate})
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

// get handles GET /api/v1/clusters/:cluster/namespaces/:namespace.
func (h *Handler) get(c *gin.Context) {
	namespaceName := c.Param("namespace")
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

	ns := &corev1.Namespace{}
	if err := adminClient.Get(c.Request.Context(), client.ObjectKey{Name: namespaceName}, ns); err != nil {
		apierrors.Respond(c, err)
		return
	}

	allowed, err := canCreateCapps(c.Request.Context(), userClient, namespaceName)
	if err != nil || !allowed {
		apierrors.Respond(c, apierrors.NewForbidden(fmt.Sprintf("not allowed to view namespace %s", namespaceName)))
		return
	}

	rq := &corev1.ResourceQuota{}
	err = adminClient.Get(c.Request.Context(), client.ObjectKey{Name: fmt.Sprintf("%s-quota", namespaceName), Namespace: namespaceName}, rq)
	if err != nil && !k8serrors.IsNotFound(err) {
		apierrors.Respond(c, err)
		return
	}

	rb := &rbacv1.RoleBinding{}
	err = adminClient.Get(c.Request.Context(), client.ObjectKey{Name: fmt.Sprintf("%s-capp-access", namespaceName), Namespace: namespaceName}, rb)
	if err != nil && !k8serrors.IsNotFound(err) {
		apierrors.Respond(c, err)
		return
	}

	quota := quotaInfoFromK8s(rq)
	users := []string{}
	for _, subject := range rb.Subjects {
		users = append(users, subject.Name)
	}
	item := NamespaceItem{Name: ns.Name, Status: string(ns.Status.Phase), Quota: quota, Users: &users}
	c.JSON(http.StatusOK, item)

}

// create handles POST /api/v1/clusters/:cluster/namespaces.

func (h *Handler) create(c *gin.Context) {
	userClient, ok := c.MustGet(string(middleware.K8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("K8sClientKey")))
		return
	}

	allowed, err := canCreateNamespaces(c.Request.Context(), userClient)
	if err != nil || !allowed {
		apierrors.Respond(c, apierrors.NewForbidden("not allowed to create namespaces"))
		return
	}

	adminClient, ok := c.MustGet(string(middleware.AdminK8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("AdminK8sClientKey")))
		return
	}

	var ns CreateNamespaceRequest
	if err := c.ShouldBindJSON(&ns); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	if err := createNamespace(c.Request.Context(), adminClient, ns); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusCreated, NamespaceItem{Name: ns.Name, Status: "Active"})
}

// update handles PUT /api/v1/clusters/:cluster/namespaces/:name.
// This will recreate the rolebinding and resourcequota based on the request body.
func (h *Handler) update(c *gin.Context) {
	userClient, ok := c.MustGet(string(middleware.K8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("K8sClientKey")))
		return
	}

	allowed, err := canCreateNamespaces(c.Request.Context(), userClient)
	if err != nil || !allowed {
		apierrors.Respond(c, apierrors.NewForbidden("not allowed to update namespaces"))
		return
	}

	adminClient, ok := c.MustGet(string(middleware.AdminK8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("AdminK8sClientKey")))
		return
	}

	req := UpdateNamespaceRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	nsName := c.Param("namespace")
	ns := &corev1.Namespace{}
	if err := adminClient.Get(c.Request.Context(), client.ObjectKey{Name: nsName}, ns); err != nil {
		apierrors.Respond(c, err)
		return
	}

	if req.Quota != nil {
		if err := updateNamespaceQuota(c.Request.Context(), adminClient, ns, *req.Quota); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	if req.Users != nil {
		if err := updateNamespaceRoleBinding(c.Request.Context(), adminClient, ns, *req.Users); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	c.JSON(http.StatusOK, NamespaceItem{Name: ns.Name, Status: string(ns.Status.Phase)})
}

func updateNamespaceQuota(ctx context.Context, adminClient client.Client, ns *corev1.Namespace, quota resourceQuota) error {
	newQuotaObj, err := generateResourceQuota(quota, ns)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}

	existingQuota := &corev1.ResourceQuota{}
	err = adminClient.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("%s-quota", ns.Name), Namespace: ns.Name}, existingQuota)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return adminClient.Create(ctx, newQuotaObj)
		}
		return err
	}

	if err := canUpdateQuota(*existingQuota, *newQuotaObj); err != nil {
		return err
	}
	newQuotaObj.ResourceVersion = existingQuota.ResourceVersion
	return adminClient.Update(ctx, newQuotaObj)
}

func updateNamespaceRoleBinding(ctx context.Context, adminClient client.Client, ns *corev1.Namespace, users []string) error {
	rbName := fmt.Sprintf("%s-capp-access", ns.Name)
	existingRB := &rbacv1.RoleBinding{}
	err := adminClient.Get(ctx, client.ObjectKey{Name: rbName, Namespace: ns.Name}, existingRB)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return adminClient.Create(ctx, generateRoleBinding(users, ns))
		}
		return err
	}

	roleBinding := generateRoleBinding(users, ns)
	roleBinding.ResourceVersion = existingRB.ResourceVersion
	return adminClient.Update(ctx, roleBinding)
}

// patch handles PATCH /api/v1/clusters/:cluster/namespaces/:namespace.
// This will update the rolebinding based on the request body.
// To update the resource quota, use the PUT endpoint with the full quota spec.
//
// Authorization: any user who can create Capps in the target namespace is
// allowed to add other users (namespace-scoped access), not just cluster admins.
func (h *Handler) patch(c *gin.Context) {

	namespaceName := c.Param("namespace")
	userClient, ok := c.MustGet(string(middleware.K8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("K8sClientKey")))
		return
	}
	allowed, err := canCreateCapps(c.Request.Context(), userClient, namespaceName)
	if err != nil || !allowed {
		apierrors.Respond(c, apierrors.NewForbidden(fmt.Sprintf("not allowed to patch namespace %s", namespaceName)))
		return
	}
	adminClient, ok := c.MustGet(string(middleware.AdminK8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("AdminK8sClientKey")))
		return
	}

	req := PatchNamespaceRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	ns := &corev1.Namespace{}
	if err := adminClient.Get(c.Request.Context(), client.ObjectKey{Name: namespaceName}, ns); err != nil {
		apierrors.Respond(c, err)
		return
	}

	if req.Users != nil {
		existingRoleBinding := &rbacv1.RoleBinding{}
		err = adminClient.Get(c.Request.Context(), client.ObjectKey{Name: fmt.Sprintf("%s-capp-access", namespaceName), Namespace: namespaceName}, existingRoleBinding)
		if err != nil {
			apierrors.Respond(c, err)
			return
		}
		existingSubjects := existingRoleBinding.Subjects
		newSubjects := existingSubjects
		for _, newUser := range *req.Users {
			found := false
			for _, subject := range existingSubjects {
				if subject.Kind == rbacv1.UserKind && subject.Name == newUser {
					found = true
					break
				}
			}
			if !found {
				newSubjects = append(newSubjects, rbacv1.Subject{
					Kind: rbacv1.UserKind,
					Name: newUser,
				})
			}
		}
		existingRoleBinding.Subjects = newSubjects
		if err := adminClient.Update(c.Request.Context(), existingRoleBinding); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	c.JSON(http.StatusOK, NamespaceItem{Name: ns.Name, Status: string(ns.Status.Phase)})
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

func createNamespace(ctx context.Context, adminClient client.Client, request CreateNamespaceRequest) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   request.Name,
			Labels: map[string]string{consts.ManagedNameSpaceLabelKey: "true"},
		},
	}

	if err := adminClient.Create(ctx, ns); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return apierrors.NewConflict("namespace", request.Name)
		}
		return err
	}

	var users []string
	if request.Users != nil {
		users = *request.Users
	}
	var quota resourceQuota
	if request.Quota != nil {
		quota = *request.Quota
	}
	return createNSResources(ctx, users, quota, ns, adminClient)
}

// createNSResources creates the resources for a namespace, including quota and role binding.
func createNSResources(ctx context.Context, users []string, quota resourceQuota, ns *corev1.Namespace, adminClient client.Client) error {
	// Create quota before RB so that if it fails, users don't get permissions to an unlimited namespace.
	if quota.CPU != "" || quota.Memory != "" || quota.Pods != 0 {

		quota, err := generateResourceQuota(quota, ns)
		if err != nil {
			return err
		}

		if err := adminClient.Create(ctx, quota); err != nil {
			return err
		}
	}

	if len(users) != 0 {
		userRoleBinding := generateRoleBinding(users, ns)
		if err := adminClient.Create(ctx, userRoleBinding); err != nil {
			return err
		}
	}
	return nil
}

func generateRoleBinding(users []string, ns *corev1.Namespace) *rbacv1.RoleBinding {
	subjects := make([]rbacv1.Subject, 0, len(users))
	for _, u := range users {
		subjects = append(subjects, rbacv1.Subject{
			Kind: rbacv1.UserKind,
			Name: u,
		})
	}

	userRoleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-capp-access", ns.Name),
			Namespace: ns.Name,
		},
		Subjects: subjects,
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     clusterRoleKind,
			Name:     clusterRoleName,
		},
	}
	return userRoleBinding
}

func generateResourceQuota(quota resourceQuota, ns *corev1.Namespace) (*corev1.ResourceQuota, error) {
	resourceList, err := generateResourceList(quota)
	if err != nil {
		return nil, fmt.Errorf("failed to generate resource list: %w", err)
	}
	quotaObj := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-quota", ns.Name),
			Namespace: ns.Name,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: resourceList,
			Scopes: []corev1.ResourceQuotaScope{
				corev1.ResourceQuotaScopeNotTerminating,
			},
		},
	}
	return quotaObj, nil
}

// generateResourceList converts the resourceQuota from the request into a Kubernetes ResourceList for the ResourceQuota spec.
func generateResourceList(quota resourceQuota) (corev1.ResourceList, error) {
	resList := corev1.ResourceList{}

	if quota.CPU != "" {
		cpu, err := resource.ParseQuantity(quota.CPU)
		if err != nil {
			return nil, fmt.Errorf("invalid CPU quota: %w", err)
		}
		resList[corev1.ResourceLimitsCPU] = cpu
	}
	if quota.Memory != "" {
		memory, err := resource.ParseQuantity(quota.Memory)
		if err != nil {
			return nil, fmt.Errorf("invalid memory quota: %w", err)
		}
		resList[corev1.ResourceLimitsMemory] = memory
	}
	if quota.Pods != 0 {
		pods, err := resource.ParseQuantity(fmt.Sprintf("%d", quota.Pods))
		if err != nil {
			return nil, fmt.Errorf("invalid pods quota: %w", err)
		}
		resList[corev1.ResourcePods] = pods
	}
	return resList, nil
}

// listQuotaInfoMap lists all ResourceQuotas cluster-wide and returns a map
// keyed by namespace. Namespaces without a quota are absent from the map.
// Errors are logged and result in an empty map rather than failing the request.
func listQuotaInfoMap(ctx context.Context, adminClient client.Client, logger *zap.Logger) map[string]*QuotaInfo {
	var quotaList corev1.ResourceQuotaList
	if err := adminClient.List(ctx, &quotaList); err != nil {
		logger.Warn("failed to list resource quotas", zap.Error(err))
		return map[string]*QuotaInfo{}
	}

	result := make(map[string]*QuotaInfo, len(quotaList.Items))
	for i := range quotaList.Items {
		q := &quotaList.Items[i]
		if _, exists := result[q.Namespace]; exists {
			continue
		}
		result[q.Namespace] = quotaInfoFromK8s(q)
	}
	return result
}

// quotaInfoFromK8s converts a Kubernetes ResourceQuota into a QuotaInfo DTO.
func quotaInfoFromK8s(quota *corev1.ResourceQuota) *QuotaInfo {
	info := &QuotaInfo{}
	if cpu, ok := quota.Spec.Hard[corev1.ResourceLimitsCPU]; ok {
		info.CPU = cpu.String()
	}
	if mem, ok := quota.Spec.Hard[corev1.ResourceLimitsMemory]; ok {
		info.Memory = mem.String()
	}
	if pods, ok := quota.Spec.Hard[corev1.ResourcePods]; ok {
		v := int(pods.Value())
		info.Pods = &v
	}

	used := &UsedQuotaInfo{}
	hasUsed := false
	if cpu, ok := quota.Status.Used[corev1.ResourceLimitsCPU]; ok {
		used.CPU = cpu.String()
		hasUsed = true
	}
	if mem, ok := quota.Status.Used[corev1.ResourceLimitsMemory]; ok {
		used.Memory = mem.String()
		hasUsed = true
	}
	if pods, ok := quota.Status.Used[corev1.ResourcePods]; ok {
		v := int(pods.Value())
		used.Pods = &v
		hasUsed = true
	}
	if hasUsed {
		info.Used = used
	}

	return info
}

// canUpdateQuota checks if the new quota is valid and can be applied to the existing quota.
// A missing resource key in new is treated as a decrease to zero.
func canUpdateQuota(existing, new corev1.ResourceQuota) error {
	zero := resource.MustParse("0")
	for resName, existingQty := range existing.Spec.Hard {
		newQty, exists := new.Spec.Hard[resName]
		if !exists {
			newQty = zero
		} else if newQty.Cmp(existingQty) >= 0 {
			continue
		}
		usedQty, usedExists := existing.Status.Used[resName]
		if usedExists && usedQty.Cmp(newQty) > 0 {
			return fmt.Errorf("cannot decrease %s quota because current usage (%s) exceeds the new quota (%s)", resName, usedQty.String(), newQty.String())
		}
	}
	return nil
}

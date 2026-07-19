package namespaces

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func managedNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{consts.ManagedNameSpaceLabelKey: "true"},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func existingRoleBinding(nsName string, users []string) *rbacv1.RoleBinding {
	subjects := make([]rbacv1.Subject, 0, len(users))
	for _, u := range users {
		subjects = append(subjects, rbacv1.Subject{Kind: rbacv1.UserKind, Name: u})
	}
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-capp-access", nsName),
			Namespace:       nsName,
			ResourceVersion: "1",
		},
		Subjects: subjects,
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     clusterRoleKind,
			Name:     clusterRoleName,
		},
	}
}

func existingResourceQuota(nsName, cpu, memory string) *corev1.ResourceQuota {
	hard := corev1.ResourceList{}
	if cpu != "" {
		hard[corev1.ResourceLimitsCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		hard[corev1.ResourceLimitsMemory] = resource.MustParse(memory)
	}
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-quota", nsName),
			Namespace:       nsName,
			ResourceVersion: "1",
		},
		Spec: corev1.ResourceQuotaSpec{Hard: hard},
	}
}

// engine builds a test engine where both user and admin client are the same
// fake client with SAR allowed.
// nolint:unparam
func engine(t *testing.T, meta cluster.ClusterMeta, adminObjects []client.Object, userObjects ...client.Object) *testutil.EngineHelper {
	return testutil.NewEngineHelperWithAdmin(t,
		testutil.FakeClientAllowSAR(t, userObjects...),
		testutil.FakeClientAllowSAR(t, adminObjects...),
		meta, New())
}

// -- List (vanilla Kubernetes) tests --

func TestList_VanillaK8s_Success(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod", IsOpenShift: false},
		[]client.Object{managedNamespace("my-ns")})
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotNil(t, resp.Items)
}

func TestList_VanillaK8s_Empty(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod", IsOpenShift: false}, nil)
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Items)
}

func TestList_VanillaK8s_ReturnsNamespaceItem(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod", IsOpenShift: false},
		[]client.Object{managedNamespace("ns-a")})
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 1)
	assert.Equal(t, "ns-a", resp.Items[0].Name)
	assert.Equal(t, "Active", resp.Items[0].Status)
}

// -- Create tests --

func TestCreate_BadJSON(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.Post("/namespaces", bytes.NewBufferString("{bad"))

	assert.True(t, w.Code >= 400)
}

func TestCreate_MinimalRequest(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	body := CreateNamespaceRequest{Name: "new-ns"}
	w := e.PostJSON("/namespaces", body)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-ns", resp.Name)
}

func TestCreate_WithUsersAndQuota(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	users := []string{"alice", "bob"}
	quota := resourceQuota{CPU: "2", Memory: "4Gi"}
	body := CreateNamespaceRequest{Name: "new-ns", Users: &users, Quota: &quota}
	w := e.PostJSON("/namespaces", body)

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreate_NilUsersAndQuota_NoNilDeref(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	// Send only required Name field — Users and Quota are nil pointers.
	w := e.PostJSON("/namespaces", map[string]string{"name": "bare-ns"})

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreate_InvalidCPUQuota(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	users := []string{"alice"}
	quota := resourceQuota{CPU: "not-a-quantity"}
	body := CreateNamespaceRequest{Name: "new-ns", Users: &users, Quota: &quota}
	w := e.PostJSON("/namespaces", body)

	assert.True(t, w.Code >= 400)
}

func TestCreate_MissingName(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.PostJSON("/namespaces", map[string]string{})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// -- Update (PUT) tests --

func TestUpdate_NsNotFound(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.PutJSON("/namespaces/missing-ns", UpdateNamespaceRequest{})

	assert.True(t, w.Code >= 400)
}

func TestUpdate_NilQuotaAndUsers_NoOp(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	w := e.PutJSON("/namespaces/my-ns", UpdateNamespaceRequest{})

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-ns", resp.Name)
}

func TestUpdate_CreatesMissingQuota(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	quota := resourceQuota{CPU: "4", Memory: "8Gi"}
	w := e.PutJSON("/namespaces/my-ns", UpdateNamespaceRequest{Quota: &quota})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_UpdatesExistingQuota(t *testing.T) {
	ns := managedNamespace("my-ns")
	rq := existingResourceQuota("my-ns", "2", "4Gi")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns, rq})
	quota := resourceQuota{CPU: "4", Memory: "8Gi"}
	w := e.PutJSON("/namespaces/my-ns", UpdateNamespaceRequest{Quota: &quota})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_CreatesMissingRoleBinding(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	users := []string{"alice"}
	w := e.PutJSON("/namespaces/my-ns", UpdateNamespaceRequest{Users: &users})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_UpdatesExistingRoleBinding(t *testing.T) {
	ns := managedNamespace("my-ns")
	rb := existingRoleBinding("my-ns", []string{"alice"})
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns, rb})
	users := []string{"bob"}
	w := e.PutJSON("/namespaces/my-ns", UpdateNamespaceRequest{Users: &users})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdate_InvalidQuota_Returns400(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	quota := resourceQuota{CPU: "not-valid"}
	w := e.PutJSON("/namespaces/my-ns", UpdateNamespaceRequest{Quota: &quota})

	assert.True(t, w.Code >= 400)
}

// -- Patch tests --

func TestPatch_BadJSON(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	w := e.Patch("/namespaces/my-ns", bytes.NewBufferString("{bad"))

	assert.True(t, w.Code >= 400)
}

func TestPatch_NsNotFound(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.PatchJSON("/namespaces/missing", PatchNamespaceRequest{})

	assert.True(t, w.Code >= 400)
}

func TestPatch_NilUsers_ReturnsOK(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	// No users field — req.Users is nil, should not panic or look up RoleBinding.
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{})

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-ns", resp.Name)
}

func TestPatch_AddsNewUser(t *testing.T) {
	ns := managedNamespace("my-ns")
	rb := existingRoleBinding("my-ns", []string{"alice"})
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns, rb})
	users := []string{"bob"}
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{Users: &users})

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-ns", resp.Name)
}

func TestPatch_SkipsDuplicateUser(t *testing.T) {
	ns := managedNamespace("my-ns")
	rb := existingRoleBinding("my-ns", []string{"alice"})
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns, rb})
	users := []string{"alice"}
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{Users: &users})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPatch_RoleBindingNotFound_Returns400(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	users := []string{"alice"}
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{Users: &users})

	assert.True(t, w.Code >= 400)
}

// -- generateRoleBinding unit tests --

func TestGenerateRoleBinding_HasAPIGroup(t *testing.T) {
	ns := managedNamespace("test-ns")
	rb := generateRoleBinding([]string{"alice"}, ns)
	assert.Equal(t, "rbac.authorization.k8s.io", rb.RoleRef.APIGroup)
	assert.Equal(t, clusterRoleKind, rb.RoleRef.Kind)
	assert.Equal(t, clusterRoleName, rb.RoleRef.Name)
}

func TestGenerateRoleBinding_Subjects(t *testing.T) {
	ns := managedNamespace("test-ns")
	rb := generateRoleBinding([]string{"alice", "bob"}, ns)
	require.Len(t, rb.Subjects, 2)
	assert.Equal(t, rbacv1.UserKind, rb.Subjects[0].Kind)
	assert.Equal(t, "alice", rb.Subjects[0].Name)
	assert.Equal(t, "bob", rb.Subjects[1].Name)
}

// -- generateResourceList unit tests --

func TestGenerateResourceList_ValidQuantities(t *testing.T) {
	list, err := generateResourceList(resourceQuota{CPU: "2", Memory: "4Gi", Pods: 10})
	require.NoError(t, err)
	assert.Contains(t, list, corev1.ResourceLimitsCPU)
	assert.Contains(t, list, corev1.ResourceLimitsMemory)
	assert.Contains(t, list, corev1.ResourcePods)
}

func TestGenerateResourceList_InvalidCPU(t *testing.T) {
	_, err := generateResourceList(resourceQuota{CPU: "not-valid"})
	assert.Error(t, err)
}

func TestGenerateResourceList_InvalidMemory(t *testing.T) {
	_, err := generateResourceList(resourceQuota{Memory: "not-valid"})
	assert.Error(t, err)
}

// -- canUpdateQuota unit tests --

func TestCanUpdateQuota_AllowsIncrease(t *testing.T) {
	existing := corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("2"),
		}},
	}
	newQ := corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("4"),
		}},
	}
	assert.NoError(t, canUpdateQuota(existing, newQ))
}

func TestCanUpdateQuota_BlocksDecreaseWhenUsed(t *testing.T) {
	existing := corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("4"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("3"),
		}},
	}
	newQ := corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("2"),
		}},
	}
	assert.Error(t, canUpdateQuota(existing, newQ))
}

func TestCanUpdateQuota_AllowsDecreaseWhenUnused(t *testing.T) {
	existing := corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("4"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("1"),
		}},
	}
	newQ := corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("2"),
		}},
	}
	assert.NoError(t, canUpdateQuota(existing, newQ))
}

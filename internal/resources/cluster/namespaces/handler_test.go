package namespaces

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func existingRoleBinding(nsName string, users []string) *rbacv1.RoleBinding { //nolint:unparam
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
		meta, New(zap.NewNop()))
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
	assert.Equal(t, 1, resp.Total)
}

func TestList_VanillaK8s_Empty(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod", IsOpenShift: false}, nil)
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Total)
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
	assert.True(t, resp.Items[0].CanEdit)
}

func TestList_VanillaK8s_IncludesUsersFromRoleBinding(t *testing.T) {
	ns := managedNamespace("ns-a")
	rb := existingRoleBinding("ns-a", []string{"alice", "bob"})
	e := engine(t, cluster.ClusterMeta{Name: "prod", IsOpenShift: false},
		[]client.Object{ns, rb})
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 1)
	require.NotNil(t, resp.Items[0].Users)
	assert.ElementsMatch(t, []string{"alice", "bob"}, *resp.Items[0].Users)
}

func TestList_VanillaK8s_CanEditFalseForNonAdmin(t *testing.T) {
	ns := managedNamespace("ns-a")
	// User has namespace access (canCreateCapps) but not admin (canCreateNamespaces)
	e := testutil.NewEngineHelperWithAdmin(t,
		testutil.FakeClientAllowNamespacedSAR(t),
		testutil.FakeClientAllowSAR(t, ns),
		cluster.ClusterMeta{Name: "prod", IsOpenShift: false}, New(zap.NewNop()))
	w := e.Get("/namespaces")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 1)
	assert.False(t, resp.Items[0].CanEdit)
	assert.False(t, resp.CanCreate)
}

// -- Get tests --

func TestGet_NsNotFound(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.Get("/namespaces/missing-ns")

	assert.GreaterOrEqual(t, w.Code, 400)
}

func TestGet_ExistingNamespace(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	w := e.Get("/namespaces/my-ns")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-ns", resp.Name)
	assert.Equal(t, "Active", resp.Status)
}

func TestGet_ExistingNamespaceWithQuotaAndUsers(t *testing.T) {
	ns := managedNamespace("my-ns")
	rq := existingResourceQuota("my-ns", "2", "4Gi")
	rb := existingRoleBinding("my-ns", []string{"alice", "bob"})
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns, rq, rb})
	w := e.Get("/namespaces/my-ns")

	assert.Equal(t, http.StatusOK, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-ns", resp.Name)
	assert.Equal(t, "Active", resp.Status)
	require.NotNil(t, resp.Quota)
	assert.Equal(t, "2", resp.Quota.CPU)
	assert.Equal(t, "4Gi", resp.Quota.Memory)
	require.NotNil(t, resp.Users)
	assert.ElementsMatch(t, []string{"alice", "bob"}, *resp.Users)
	assert.True(t, resp.CanEdit)
}

// -- Create tests --

func TestCreate_BadJSON(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.Post("/namespaces", bytes.NewBufferString("{bad"))

	assert.GreaterOrEqual(t, w.Code, 400)
}

func TestCreate_MinimalRequest(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	body := CreateNamespaceRequest{Name: "new-ns"}
	w := e.PostJSON("/namespaces", body)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-ns", resp.Name)
	assert.True(t, resp.CanEdit)
	assert.Nil(t, resp.Users)
	assert.Nil(t, resp.Quota)
}

func TestCreate_WithUsersAndQuota(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	users := []string{"alice", "bob"}
	quota := resourceQuota{CPU: "2", Memory: "4Gi"}
	body := CreateNamespaceRequest{Name: "new-ns", Users: &users, Quota: &quota}
	w := e.PostJSON("/namespaces", body)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-ns", resp.Name)
	assert.True(t, resp.CanEdit)
	require.NotNil(t, resp.Users)
	assert.ElementsMatch(t, []string{"alice", "bob"}, *resp.Users)
	require.NotNil(t, resp.Quota)
	assert.Equal(t, "2", resp.Quota.CPU)
	assert.Equal(t, "4Gi", resp.Quota.Memory)
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

	assert.GreaterOrEqual(t, w.Code, 400)
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

	assert.GreaterOrEqual(t, w.Code, 400)
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
	var resp NamespaceItem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.CanEdit)
	require.NotNil(t, resp.Quota)
	assert.Equal(t, "4", resp.Quota.CPU)
	assert.Equal(t, "8Gi", resp.Quota.Memory)
	assert.Nil(t, resp.Quota.Pods)
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

	assert.GreaterOrEqual(t, w.Code, 400)
}

// -- Patch tests --

func TestPatch_BadJSON(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	w := e.Patch("/namespaces/my-ns", bytes.NewBufferString("{bad"))

	assert.GreaterOrEqual(t, w.Code, 400)
}

func TestPatch_NsNotFound(t *testing.T) {
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
	w := e.PatchJSON("/namespaces/missing", PatchNamespaceRequest{})

	assert.GreaterOrEqual(t, w.Code, 400)
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
	require.NotNil(t, resp.Users)
	assert.ElementsMatch(t, []string{"alice", "bob"}, *resp.Users)
	assert.True(t, resp.CanEdit)
}

func TestPatch_SkipsDuplicateUser(t *testing.T) {
	ns := managedNamespace("my-ns")
	rb := existingRoleBinding("my-ns", []string{"alice"})
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns, rb})
	users := []string{"alice"}
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{Users: &users})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPatch_NamespaceScopedUser_CanAddUsers(t *testing.T) {
	ns := managedNamespace("my-ns")
	rb := existingRoleBinding("my-ns", []string{"alice"})
	// User client allows namespace-scoped SARs (canCreateCapps) but denies
	// cluster-scoped SARs (canCreateNamespaces). This simulates a non-admin
	// user who has access to the namespace.
	e := testutil.NewEngineHelperWithAdmin(t,
		testutil.FakeClientAllowNamespacedSAR(t),
		testutil.FakeClientAllowSAR(t, ns, rb),
		cluster.ClusterMeta{Name: "prod"}, New(zap.NewNop()))
	users := []string{"bob"}
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{Users: &users})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPatch_UnauthorizedUser_Returns403(t *testing.T) {
	ns := managedNamespace("my-ns")
	rb := existingRoleBinding("my-ns", []string{"alice"})
	// User client denies all SARs — user has no access at all.
	e := testutil.NewEngineHelperWithAdmin(t,
		testutil.FakeClientDenySAR(t),
		testutil.FakeClientAllowSAR(t, ns, rb),
		cluster.ClusterMeta{Name: "prod"}, New(zap.NewNop()))
	users := []string{"bob"}
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{Users: &users})

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPatch_RoleBindingNotFound_Returns400(t *testing.T) {
	ns := managedNamespace("my-ns")
	e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
	users := []string{"alice"}
	w := e.PatchJSON("/namespaces/my-ns", PatchNamespaceRequest{Users: &users})

	assert.GreaterOrEqual(t, w.Code, 400)
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

func intPtr(v int) *int { return &v }

func TestQuotaInfoFromK8s(t *testing.T) {
	tests := []struct {
		name     string
		quota    *corev1.ResourceQuota
		expected *QuotaInfo
	}{
		{
			name: "full hard limits and usage",
			quota: &corev1.ResourceQuota{
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
					corev1.ResourceLimitsCPU:    resource.MustParse("4"),
					corev1.ResourceLimitsMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:         resource.MustParse("50"),
				}},
				Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
					corev1.ResourceLimitsCPU:    resource.MustParse("1500m"),
					corev1.ResourceLimitsMemory: resource.MustParse("3Gi"),
					corev1.ResourcePods:         resource.MustParse("10"),
				}},
			},
			expected: &QuotaInfo{
				CPU: "4", Memory: "8Gi", Pods: intPtr(50),
				Used: &UsedQuotaInfo{CPU: "1500m", Memory: "3Gi", Pods: intPtr(10)},
			},
		},
		{
			name: "hard limits only, no usage",
			quota: &corev1.ResourceQuota{
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
					corev1.ResourceLimitsCPU:    resource.MustParse("2"),
					corev1.ResourceLimitsMemory: resource.MustParse("4Gi"),
				}},
			},
			expected: &QuotaInfo{CPU: "2", Memory: "4Gi"},
		},
		{
			name: "pods hard limit of zero serializes correctly",
			quota: &corev1.ResourceQuota{
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
					corev1.ResourcePods: resource.MustParse("0"),
				}},
			},
			expected: &QuotaInfo{Pods: intPtr(0)},
		},
		{
			name: "partial usage (only cpu)",
			quota: &corev1.ResourceQuota{
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
					corev1.ResourceLimitsCPU: resource.MustParse("4"),
				}},
				Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
					corev1.ResourceLimitsCPU: resource.MustParse("500m"),
				}},
			},
			expected: &QuotaInfo{
				CPU:  "4",
				Used: &UsedQuotaInfo{CPU: "500m"},
			},
		},
		{
			name: "empty quota spec",
			quota: &corev1.ResourceQuota{
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{}},
			},
			expected: &QuotaInfo{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := quotaInfoFromK8s(tt.quota)
			assert.Equal(t, tt.expected.CPU, result.CPU)
			assert.Equal(t, tt.expected.Memory, result.Memory)
			assert.Equal(t, tt.expected.Pods, result.Pods)
			if tt.expected.Used == nil {
				assert.Nil(t, result.Used)
			} else {
				require.NotNil(t, result.Used)
				assert.Equal(t, tt.expected.Used.CPU, result.Used.CPU)
				assert.Equal(t, tt.expected.Used.Memory, result.Used.Memory)
				assert.Equal(t, tt.expected.Used.Pods, result.Used.Pods)
			}
		})
	}
}

// -- Delete tests --

func TestDelete(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		ns := managedNamespace("my-ns")
		e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns})
		w := e.Delete("/namespaces/my-ns")

		assert.Equal(t, http.StatusNoContent, w.Code)
	})

	t.Run("not found", func(t *testing.T) {
		e := engine(t, cluster.ClusterMeta{Name: "prod"}, nil)
		w := e.Delete("/namespaces/missing-ns")

		assert.GreaterOrEqual(t, w.Code, 400)
	})

	t.Run("blocked by capp", func(t *testing.T) {
		ns := managedNamespace("my-ns")
		capp := &cappv1alpha1.Capp{
			ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "my-ns"},
		}
		e := engine(t, cluster.ClusterMeta{Name: "prod"}, []client.Object{ns, capp})
		w := e.Delete("/namespaces/my-ns")

		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Contains(t, w.Body.String(), "contains")
	})

	t.Run("unauthorized", func(t *testing.T) {
		ns := managedNamespace("my-ns")
		e := testutil.NewEngineHelperWithAdmin(t,
			testutil.FakeClientDenySAR(t),
			testutil.FakeClientAllowSAR(t, ns),
			cluster.ClusterMeta{Name: "prod"}, New(zap.NewNop()))
		w := e.Delete("/namespaces/my-ns")

		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestGet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		actions   []string
		namespace string
		uid       string
		status    int
		canEdit   bool
		invalid   bool
	}{
		{name: "visible with both edit permissions", actions: []string{"get", "update", "replace"}, status: http.StatusOK, canEdit: true},
		{name: "update alone cannot edit users", actions: []string{"get", "update"}, status: http.StatusOK},
		{name: "replace alone cannot edit quota", actions: []string{"get", "replace"}, status: http.StatusOK},
		{name: "denied before quota", actions: []string{"update", "replace"}, status: http.StatusForbidden},
		{name: "protected before details", actions: []string{"get"}, namespace: "policies", status: http.StatusForbidden},
		{name: "configured exclusion", actions: []string{"get"}, namespace: "excluded", status: http.StatusForbidden},
		{name: "cached miss", actions: []string{"get"}, namespace: "missing", status: http.StatusNotFound},
		{name: "replacement does not inherit users", actions: []string{"get"}, uid: "replacement", status: http.StatusOK},
		{name: "invalidated generation", actions: []string{"get"}, invalid: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ns := managedNamespace("team")
				ns.UID = "original"
				if tc.uid != "" {
					ns.UID = types.UID(tc.uid)
				}
				rules := make([]map[string]any, 0, len(tc.actions))
				for _, action := range tc.actions {
					resource := "namespaces"
					if action == "replace" {
						resource = "roles/capp-user"
					}
					rules = append(rules, map[string]any{"resource": resource, "actions": []string{action}})
				}
				payload, err := json.Marshal(map[string]any{
					"version": 1, "roles": map[string]any{"capp-user": rules},
					"bindings": []map[string]string{{"kind": "Group", "name": "readers", "role": "capp-user", "scope": "allNamespaces"}},
				})
				require.NoError(t, err)
				policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "capp-policy", Namespace: "policies", Labels: map[string]string{consts.CappAPIGroup + "/authorization": "true"}}, Data: map[string]string{"policy.json": string(payload)}}
				membership := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
					Name: "capp-members-original", Namespace: "policies", Labels: policy.Labels,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "team", UID: "original"}},
				}, Data: map[string]string{"policy.json": `{"version":1,"namespace":{"name":"team","uid":"original"},"users":["zoe","alice"],"bindings":[{"kind":"User","name":"operator","role":"capp-user"}]}`}}
				base := testutil.FakeClient(t, ns, policy, membership, existingResourceQuota("team", "2", "4Gi"))
				warm, quotaReads := false, 0
				watchClient, ok := base.(client.WithWatch)
				require.True(t, ok)
				target := interceptor.NewClient(watchClient, interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if warm && tc.invalid {
							return errors.New("offline")
						}
						if warm {
							t.Fatalf("unexpected request LIST %T", list)
						}
						if err := c.List(ctx, list, opts...); err != nil {
							return err
						}
						list.SetResourceVersion("1")
						return nil
					},
					Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
						return watch.NewRaceFreeFake(), nil
					},
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.ResourceQuota); !ok {
							t.Fatalf("unexpected GET %T", obj)
						}
						quotaReads++
						return c.Get(ctx, key, obj, opts...)
					},
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						return errors.New("unexpected write or SSAR")
					},
				})
				cfg := &config.Config{Authorization: config.AuthorizationConfig{Mode: config.AuthorizationModeApplication}, Clusters: []config.ClusterConfig{{Name: "prod", Authorization: config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}}}}
				store, err := authorization.NewStore(target, cfg.Clusters[0].Authorization, zap.NewNop())
				require.NoError(t, err)
				defer store.Stop()
				require.NoError(t, store.Start(t.Context()))
				synctest.Wait()
				time.Sleep(time.Second)
				synctest.Wait()
				warm = true
				if tc.namespace == "missing" {
					// A live namespace absent from the captured collection is still a miss.
					require.NoError(t, base.Create(t.Context(), managedNamespace("missing")))
				}
				if tc.invalid {
					store.Invalidate()
				}
				mgr := &testutil.MockClusterManager{
					GetFn:                func(string) (*cluster.ClusterClient, error) { return &cluster.ClusterClient{}, nil },
					ServiceClientForFn:   func(*cluster.ClusterClient) (client.WithWatch, error) { return target, nil },
					IsNamespaceAllowedFn: func(_ *cluster.ClusterClient, name string) bool { return name != "excluded" },
				}
				router := gin.New()
				router.Use(func(c *gin.Context) {
					c.Set(string(middleware.CredentialKey), auth.ClusterCredential{ImpersonateUser: "alice", ImpersonateGroups: []string{"readers"}})
				})
				router.Use(middleware.Authorization(cfg, mgr, map[string]*authorization.Store{"prod": store}))
				router.GET("/api/v1/clusters/:cluster/namespaces/:namespace", New(zap.NewNop()).get)
				name := tc.namespace
				if name == "" {
					name = "team"
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/prod/namespaces/"+name, nil))
				require.Equal(t, tc.status, response.Code, response.Body.String())
				if tc.status != http.StatusOK {
					require.Zero(t, quotaReads)
					return
				}
				require.Equal(t, 1, quotaReads)
				var item NamespaceItem
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &item))
				assert.Equal(t, tc.canEdit, item.CanEdit)
				assert.Equal(t, "team", item.Name)
				require.NotNil(t, item.Quota)
				assert.Equal(t, "2", item.Quota.CPU)
				require.NotNil(t, item.Users)
				want := []string{"alice", "zoe"}
				if tc.uid != "" {
					want = []string{}
				}
				assert.Equal(t, want, *item.Users)
			})
		})
	}
}

func TestList(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count        int
		username     string
		invalidate   string
		quotaFailure bool
	}{
		{name: "filtered discovery", count: 2, username: "alice"},
		{name: "read cost stays bounded", count: 30, username: "alice"},
		{name: "no visibility grants", count: 2, username: "outsider"},
		{name: "create permission without namespace visibility", count: 2, username: "creator"},
		{name: "no managed namespaces", username: "alice"},
		{name: "policy unavailable even for empty results", username: "alice", invalidate: "policy"},
		{name: "namespace collection unavailable even for empty results", username: "alice", invalidate: "namespaces"},
		{name: "quota failure preserves authorized metadata", count: 2, username: "alice", quotaFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newNamespaceListFixture(t, tc.count)
				f.quotaFailure = tc.quotaFailure
				snapshot, err := f.store.Snapshot(auth.ClusterCredential{ImpersonateUser: tc.username})
				require.NoError(t, err)
				switch tc.invalidate {
				case "policy":
					f.store.Invalidate()
				case "namespaces":
					f.store.InvalidateNamespaces()
				}
				response := f.serve(snapshot)
				if tc.invalidate != "" {
					require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
					require.Contains(t, response.Body.String(), `"reason":"POLICY_UNAVAILABLE"`)
					require.Zero(t, f.quotaLists)
					return
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var result NamespaceListResponse
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
				wantCount := 0
				if tc.username == "alice" {
					wantCount = tc.count
				}
				require.NotNil(t, result.Items)
				require.Len(t, result.Items, wantCount)
				assert.Equal(t, wantCount, result.Total)
				assert.Equal(t, tc.username == "creator", result.CanCreate)
				wantQuotaLists := 0
				if wantCount > 0 {
					wantQuotaLists = 1
				}
				assert.Equal(t, wantQuotaLists, f.quotaLists)
				for i, item := range result.Items {
					assert.Equal(t, fmt.Sprintf("team-%02d", i), item.Name)
					assert.Equal(t, string(corev1.NamespaceActive), item.Status)
					require.NotNil(t, item.Users)
					assert.Equal(t, []string{"alice"}, *item.Users)
					assert.Equal(t, i == 0, item.CanEdit)
					if tc.quotaFailure {
						assert.Nil(t, item.Quota)
					} else {
						require.NotNil(t, item.Quota)
						assert.Equal(t, "3", item.Quota.CPU)
						assert.Equal(t, "8Gi", item.Quota.Memory)
					}
				}
			})
		})
	}

	t.Run("recreated namespace uses old identity only until convergence", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newNamespaceListFixture(t, 1)
			credential := auth.ClusterCredential{ImpersonateUser: "alice"}
			captured, err := f.store.Snapshot(credential)
			require.NoError(t, err)
			var namespace corev1.Namespace
			require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Name: "team-00"}, &namespace))
			require.NoError(t, f.client.Delete(t.Context(), &namespace))
			namespace.UID = "replacement"
			namespace.ResourceVersion = ""
			require.NoError(t, f.client.Create(t.Context(), &namespace))
			before := f.serve(captured)
			require.Equal(t, http.StatusOK, before.Code)
			var result NamespaceListResponse
			require.NoError(t, json.Unmarshal(before.Body.Bytes(), &result))
			require.Len(t, result.Items, 1)
			require.Equal(t, []string{"alice"}, *result.Items[0].Users)

			f.namespaceWatch.Modify(&namespace)
			synctest.Wait()
			current, err := f.store.Snapshot(credential)
			require.NoError(t, err)
			cached, err := current.Namespace(namespace.Name)
			require.NoError(t, err)
			require.Equal(t, namespace.UID, cached.UID)
			after := f.serve(current)
			require.Equal(t, http.StatusOK, after.Code)
			require.NoError(t, json.Unmarshal(after.Body.Bytes(), &result))
			require.Empty(t, result.Items)
			require.Zero(t, result.Total)
			retired := f.serve(captured)
			require.Equal(t, http.StatusServiceUnavailable, retired.Code)
			require.Contains(t, retired.Body.String(), `"reason":"POLICY_UNAVAILABLE"`)
		})
	})
}

type namespaceListFixture struct {
	store          *authorization.Store
	client         client.Client
	target         client.WithWatch
	config         config.ClusterAuthorizationConfig
	meta           cluster.ClusterMeta
	namespaceWatch *watch.RaceFreeFakeWatcher
	request        context.Context
	quotaLists     int
	quotaFailure   bool
}

func newNamespaceListFixture(t *testing.T, count int) *namespaceListFixture {
	t.Helper()
	f := &namespaceListFixture{
		request: t.Context(),
		config:  config.ClusterAuthorizationConfig{PolicyNamespace: "policies", ProtectedNamespaces: []string{"protected"}},
		meta:    cluster.ClusterMeta{Name: "prod", AllowedNamespaces: []string{"policies", "protected", "unmanaged", "denied"}},
	}
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "capp-policy", Namespace: "policies", Labels: map[string]string{consts.CappAPIGroup + "/authorization": "true"},
	}, Data: map[string]string{"policy.json": `{"version":1,"roles":{"capp-user":[{"resource":"namespaces","actions":["get"]}],"editor":[{"resource":"namespaces","actions":["update"]},{"resource":"roles/capp-user","actions":["replace"]}],"creator":[{"resource":"namespaces","actions":["create"]}]},"bindings":[{"kind":"User","name":"creator","role":"creator","scope":"cluster"}]}`}}
	objects := []client.Object{policy}
	names := make([]string, 0, 5+count)
	names = append(names, "policies", "protected", "excluded", "unmanaged", "denied")
	for i := range count {
		name := fmt.Sprintf("team-%02d", i)
		names = append(names, name)
		f.meta.AllowedNamespaces = append(f.meta.AllowedNamespaces, name)
	}
	for _, name := range names {
		namespace := managedNamespace(name)
		namespace.UID = types.UID("uid-" + name)
		if name == "unmanaged" {
			namespace.Labels = nil
		}
		objects = append(objects, namespace, existingResourceQuota(name, "3", "8Gi"))
		if name == "denied" {
			continue
		}
		bindings := []map[string]string{}
		if name == "team-00" {
			bindings = append(bindings, map[string]string{"kind": "User", "name": "alice", "role": "editor"})
		}
		payload, err := json.Marshal(map[string]any{
			"version": 1, "namespace": map[string]string{"name": name, "uid": string(namespace.UID)}, "users": []string{"alice"}, "bindings": bindings,
		})
		require.NoError(t, err)
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "capp-members-" + string(namespace.UID), Namespace: "policies", Labels: policy.Labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: name, UID: namespace.UID}},
		}, Data: map[string]string{"policy.json": string(payload)}})
	}
	f.client = testutil.FakeClient(t, objects...)
	base, ok := f.client.(client.WithWatch)
	require.True(t, ok)
	f.target = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.ResourceQuotaList); ok {
				f.quotaLists++
				if f.quotaFailure {
					return errors.New("quota unavailable")
				}
			} else if ctx == f.request {
				t.Fatalf("request performed authorization read: %T", list)
			}
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			list.SetResourceVersion("1")
			return nil
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("unexpected request GET")
			return nil
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("unexpected request write or SSAR")
			return nil
		},
		Watch: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
			stream := watch.NewRaceFreeFake()
			if _, ok := list.(*corev1.NamespaceList); ok {
				f.namespaceWatch = stream
			}
			return stream, nil
		},
	})
	var err error
	f.store, err = authorization.NewStore(f.target, f.config, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(f.store.Stop)
	require.NoError(t, f.store.Start(t.Context()))
	synctest.Wait()
	time.Sleep(time.Second)
	synctest.Wait()
	return f
}

func (f *namespaceListFixture) serve(snapshot *authorization.Snapshot) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ClusterMetaKey), f.meta)
		c.Set(string(middleware.AuthorizationKey), &middleware.AuthorizationState{Targets: map[string]*middleware.AuthorizationTarget{
			"prod": {Snapshot: snapshot, Client: f.target, Config: f.config},
		}})
	})
	router.GET("/api/v1/clusters/:cluster/namespaces", New(zap.NewNop()).list)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/prod/namespaces", nil).WithContext(f.request))
	return response
}

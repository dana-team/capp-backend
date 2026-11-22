package namespaces

import (
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func managedNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
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
			Name:            nsName + quotaNameSuffix,
			Namespace:       nsName,
			ResourceVersion: "1",
		},
		Spec: corev1.ResourceQuotaSpec{Hard: hard},
	}
}

// -- generateResourceList unit tests --

func TestGenerateResourceList(t *testing.T) {
	t.Run("ValidQuantities", func(t *testing.T) {
		list, err := generateResourceList(resourceQuota{CPU: "2", Memory: "4Gi", Pods: 10})
		require.NoError(t, err)
		assert.Contains(t, list, corev1.ResourceLimitsCPU)
		assert.Contains(t, list, corev1.ResourceLimitsMemory)
		assert.Contains(t, list, corev1.ResourcePods)
	})
	t.Run("InvalidCPU", func(t *testing.T) {
		_, err := generateResourceList(resourceQuota{CPU: "not-valid"})
		assert.Error(t, err)
	})
	t.Run("InvalidMemory", func(t *testing.T) {
		_, err := generateResourceList(resourceQuota{Memory: "not-valid"})
		assert.Error(t, err)
	})
}

// -- canUpdateQuota unit tests --

func TestCanUpdateQuota(t *testing.T) {
	t.Run("AllowsIncrease", func(t *testing.T) {
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
	})
	t.Run("BlocksDecreaseWhenUsed", func(t *testing.T) {
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
	})
	t.Run("AllowsDecreaseWhenUnused", func(t *testing.T) {
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
	})
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

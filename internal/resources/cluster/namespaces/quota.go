package namespaces

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const quotaNameSuffix = "-quota"

func generateResourceQuota(quota resourceQuota, ns *corev1.Namespace) (*corev1.ResourceQuota, error) {
	resourceList, err := generateResourceList(quota)
	if err != nil {
		return nil, fmt.Errorf("failed to generate resource list: %w", err)
	}
	quotaObj := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ns.Name + quotaNameSuffix,
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

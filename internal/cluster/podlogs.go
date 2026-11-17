package cluster

import (
	"context"
	"sync"

	"github.com/dana-team/capp-backend/internal/auth"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// PodLogs reads the end of one container's log. The controller-runtime
// client cannot: a pod log is a byte stream, not an object.
type PodLogs func(ctx context.Context, namespace, pod, container string, tailLines, limitBytes int64) (string, error)

// PodLogsFor returns a PodLogs that reads with the caller's credential, so
// the cluster's own RBAC decides whose logs a caller can read. The typed
// client is built on the first read and used for the others, because most
// requests never read a log and one request can read many.
func PodLogsFor(cluster *ClusterClient, cred auth.ClusterCredential) PodLogs {
	typed := sync.OnceValues(func() (kubernetes.Interface, error) {
		return kubernetes.NewForConfig(requestConfig(cluster, cred))
	})
	return func(ctx context.Context, namespace, pod, container string, tailLines, limitBytes int64) (string, error) {
		clientset, err := typed()
		if err != nil {
			return "", err
		}
		raw, err := clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
			Container:  container,
			TailLines:  &tailLines,
			LimitBytes: &limitBytes,
		}).DoRaw(ctx)
		return string(raw), err
	}
}

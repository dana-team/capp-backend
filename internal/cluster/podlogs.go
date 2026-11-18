package cluster

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// PodLogs reads the end of one container's log. The controller-runtime
// client cannot: a pod log is a byte stream, not an object.
type PodLogs func(ctx context.Context, namespace, pod, container string, tailLines, limitBytes int64) (string, error)

// PodStream watches pods and follows container logs. Both are streams, which
// the controller-runtime client the handlers get does not offer.
type PodStream interface {
	// WatchPods watches the pods of namespace that match the label
	// selector. It starts with no resourceVersion, so the API server sends
	// an ADDED event for every pod that exists now before the changes.
	WatchPods(ctx context.Context, namespace, labelSelector string) (watch.Interface, error)
	// FollowLog follows one container's log from where start says, each
	// line prefixed with an RFC 3339 timestamp. The stream ends when the
	// container stops or ctx is done.
	FollowLog(ctx context.Context, namespace, pod, container string, start LogStart) (io.ReadCloser, error)
}

// LogStart says where a followed log begins: its last TailLines lines, or
// the lines from SinceTime on. Set one of them.
type LogStart struct {
	TailLines *int64
	// SinceTime goes to the API server with second precision, so the log
	// can start up to a second before it.
	SinceTime *time.Time
}

// typedClientFor builds the typed client on the first use and reuses it,
// because most requests never read a log and one request can read many.
func typedClientFor(cluster *ClusterClient, cred auth.ClusterCredential) func() (kubernetes.Interface, error) {
	return sync.OnceValues(func() (kubernetes.Interface, error) {
		return kubernetes.NewForConfig(requestConfig(cluster, cred))
	})
}

// PodLogsFor returns a PodLogs that reads with the caller's credential, so
// the cluster's own RBAC decides whose logs a caller can read.
func PodLogsFor(cluster *ClusterClient, cred auth.ClusterCredential) PodLogs {
	typed := typedClientFor(cluster, cred)
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

// PodStreamFor returns a PodStream that uses the caller's credential, so the
// cluster's own RBAC decides whose pods and logs a caller can follow.
func PodStreamFor(cluster *ClusterClient, cred auth.ClusterCredential) PodStream {
	return podStream{typed: typedClientFor(cluster, cred)}
}

type podStream struct {
	typed func() (kubernetes.Interface, error)
}

func (p podStream) WatchPods(ctx context.Context, namespace, labelSelector string) (watch.Interface, error) {
	clientset, err := p.typed()
	if err != nil {
		return nil, err
	}
	return clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{LabelSelector: labelSelector})
}

func (p podStream) FollowLog(ctx context.Context, namespace, pod, container string, start LogStart) (io.ReadCloser, error) {
	clientset, err := p.typed()
	if err != nil {
		return nil, err
	}
	opts := &corev1.PodLogOptions{
		Container:  container,
		Follow:     true,
		Timestamps: true,
		TailLines:  start.TailLines,
	}
	if start.SinceTime != nil {
		since := metav1.NewTime(*start.SinceTime)
		opts.SinceTime = &since
	}
	return clientset.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

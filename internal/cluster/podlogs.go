package cluster

import "context"

// PodLogs reads the end of one container's log. The controller-runtime
// client cannot: a pod log is a byte stream, not an object.
type PodLogs func(ctx context.Context, namespace, pod, container string, tailLines, limitBytes int64) (string, error)

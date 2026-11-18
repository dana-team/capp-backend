package capps

// The log stream of a Capp (GET .../capps/:name/logs) is a text/event-stream
// response. Each event's data is one of the JSON objects below; the SSE
// event name says which.
const (
	// LogEventLine carries a LogLine.
	LogEventLine = "log"
	// LogEventPodStarted carries a LogPodEvent: a pod's log is now followed.
	LogEventPodStarted = "pod-started"
	// LogEventPodEnded carries a LogPodEvent: a pod's log is no longer
	// followed, because the container stopped or the pod went away.
	LogEventPodEnded = "pod-ended"
	// LogEventNotice carries a LogNotice that does not end the stream, such
	// as a pod left out because the stream follows the maximum already.
	LogEventNotice = "notice"
	// LogEventError carries a LogNotice; the stream ends after it.
	LogEventError = "error"
	// LogEventEnd carries a LogNotice; the server closes the stream after
	// it, and a client that still wants logs connects again.
	LogEventEnd = "end"
)

// LogLine is one line of a container's log.
type LogLine struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Timestamp is the time the kubelet recorded the line (RFC 3339), empty
	// when the line came without one.
	Timestamp string `json:"timestamp,omitempty"`
	Line      string `json:"line"`
}

// LogPodEvent tells that the stream started or stopped following a pod.
type LogPodEvent struct {
	Pod    string `json:"pod"`
	Reason string `json:"reason,omitempty"`
}

// LogNotice is a message about the stream itself.
type LogNotice struct {
	Message string `json:"message"`
}

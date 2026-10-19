package benchmarks

// TriggerRequest selects which baked-in check to run.
type TriggerRequest struct {
	Type Type `json:"type"`
}

// TriggerResponse is returned after a benchmark Job is created.
type TriggerResponse struct {
	CappName string `json:"cappName"`
	JobName  string `json:"jobName"`
	Type     Type   `json:"type"`
}

const (
	// StatusRunning means the Job has not finished.
	StatusRunning = "running"
	// StatusCompleted means the Job's Complete condition is true.
	StatusCompleted = "completed"
	// StatusFailed means the Job's Failed condition is true.
	StatusFailed = "failed"
)

// StatusResponse is returned by GET .../benchmark.
// JobName and Status are empty when no benchmark Job exists.
type StatusResponse struct {
	JobName string `json:"jobName"`
	Status  string `json:"status"`
}

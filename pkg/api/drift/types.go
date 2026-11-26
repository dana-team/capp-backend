// Package drift holds the drift DTOs of the REST API.
//
// It has no server dependencies, so clients (cappctl, capp-mcp) can share
// these types without pulling in Kubernetes, Gin or MongoDB.
package drift

// Drift states. A resource response has no drift object at all when desired
// state is disabled or the object is being deleted.
const (
	// StateInSync means the live object matches its recorded desired state.
	StateInSync = "inSync"
	// StateChanged means both exist and differ.
	StateChanged = "changed"
	// StateMissing means a desired state is recorded but the object is gone.
	StateMissing = "missing"
	// StateUnmanaged means the object exists but has no recorded desired state.
	StateUnmanaged = "unmanaged"
	// StateUnavailable means the desired state could not be read.
	StateUnavailable = "unavailable"
)

// Values of the onDrift query parameter of requests that update an object.
const (
	// OnDriftAccept updates the object starting from its live version.
	OnDriftAccept = "accept"
	// OnDriftRevert updates the object starting from its desired state.
	OnDriftRevert = "revert"
)

// Drift compares a live object with its recorded desired state.
type Drift struct {
	State string `json:"state"`

	// Generation and LiveVersion identify what was compared. A revert or
	// accept request sends both back, so it acts only on what the caller saw.
	Generation  int64  `json:"generation,omitempty"`
	LiveVersion string `json:"liveVersion,omitempty"`

	// Fields lists the differences when State is "changed".
	Fields []Field `json:"fields,omitempty"`

	// UpdatedBy and UpdatedAt describe the last write of the desired state.
	// UpdatedBy is empty when the caller's username is not known.
	UpdatedBy string `json:"updatedBy,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// Field is one difference. Path is a JSON pointer. A missing Desired or Live
// means the field is unset on that side. Secrets never carry values.
type Field struct {
	Path    string `json:"path"`
	Desired any    `json:"desired,omitempty"`
	Live    any    `json:"live,omitempty"`
}

// Missing is a recorded object that no longer exists in the cluster.
type Missing struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Drift     Drift  `json:"drift"`
}

// ActionRequest is the body of the revert and accept endpoints.
type ActionRequest struct {
	Generation  int64  `json:"generation"`
	LiveVersion string `json:"liveVersion,omitempty"`
}

// ActionResponse reports the drift that remains after a revert or accept.
// Drift is omitted when nothing is left to compare.
type ActionResponse struct {
	Drift *Drift `json:"drift,omitempty"`
}

// AdoptedItem is one object the adopt endpoint started tracking.
type AdoptedItem struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// AdoptResponse is returned by the adopt endpoint.
type AdoptResponse struct {
	Adopted []AdoptedItem `json:"adopted"`
	Total   int           `json:"total"`
}

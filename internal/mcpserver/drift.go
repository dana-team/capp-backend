package mcpserver

import "net/url"

// OnDriftInput is embedded by the tools that update an object.
type OnDriftInput struct {
	OnDrift string `json:"onDrift,omitempty" jsonschema:"only needed when the call fails with DRIFT_DETECTED because the live object differs from its recorded desired state (see drift in the get tool's result): accept keeps the live changes and revert discards them. Ask the user which one to use. Values sent in this call always win, so with revert send the desired values and not the live ones"`
}

func (in OnDriftInput) query() string {
	if in.OnDrift == "" {
		return ""
	}
	return "?" + url.Values{"onDrift": {in.OnDrift}}.Encode()
}

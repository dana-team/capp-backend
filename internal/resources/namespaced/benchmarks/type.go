package benchmarks

// Type selects which script baked into the runner image the Job executes.
type Type string

const (
	// TypeLatency runs the k6 latency check.
	TypeLatency Type = "latency"
	// TypeThroughput runs the k6 throughput check.
	TypeThroughput Type = "throughput"
	// TypeScale runs the scale-out check.
	TypeScale Type = "scale"
)

func (t Type) valid() bool {
	switch t {
	case TypeLatency, TypeThroughput, TypeScale:
		return true
	default:
		return false
	}
}

package usage

// StreamSuffix is appended to the installation's Valkey namespace to name
// the request metadata stream.
const StreamSuffix = "request-metadata"

func StreamName(prefix string) string { return prefix + StreamSuffix }

const (
	// ReplayHorizonDays bounds how late a delivery may still be accounted for.
	// An event older than this cannot be added to aggregates it no longer
	// belongs to, so it is rejected and recorded as a gap instead.
	ReplayHorizonDays = 7
	// FutureSkewMinutes is the clock skew tolerated on an event observed in the
	// future; anything further ahead is rejected rather than trusted.
	FutureSkewMinutes = 5
)

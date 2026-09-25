package interaction

// LimitCategory classifies a bounded proxy-local resource for exhaustion
// reporting, so a local ceiling is attributed to the resource it actually
// guards — bytes of retained representation, aggregate admitted event work,
// a lifetime or deadline, or a durable persistence bound — instead of being
// reported as provider ill health or a provider protocol defect.
type LimitCategory string

const (
	// LimitBytes bounds a byte representation: retained native dependency,
	// projected delivery, transient event memory, or a per-event/frame or
	// response size ceiling.
	LimitBytes LimitCategory = "bytes"
	// LimitEventWork bounds the aggregate admitted-event work of a stream:
	// transport, parse and projection cost spent per admitted event,
	// including events the grammar drops (such as pings).
	LimitEventWork LimitCategory = "event_work"
	// LimitTime bounds a lifetime or deadline, such as the stream cap.
	LimitTime LimitCategory = "time"
	// LimitPersistence bounds a durable payload or storage ceiling.
	LimitPersistence LimitCategory = "persistence"
)

// Exhaustion reports one bounded proxy-local resource reaching its ceiling.
// It is explicit: the named budget, its category and its configured limit are
// carried so callers can cancel upstream work where possible, withhold unsafe
// actions and attribute the failure without feeding provider health circuits.
// It is deliberately not an *Error (Incompatibility): exhaustion is not a
// contract violation by the provider.
type Exhaustion struct {
	// Resource names the exhausted budget, e.g. "retained_dependency_bytes".
	Resource string
	// Category is the limit classification for exhaustion reporting.
	Category LimitCategory
	// Limit is the configured ceiling in the resource's unit.
	Limit int
}

func (e *Exhaustion) Error() string {
	return "bounded " + string(e.Category) + " resource exhausted: " + e.Resource
}

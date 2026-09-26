package runtime

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

// Attempt is one candidate in deterministic order.
type Attempt struct {
	TargetID           string
	ProviderID         string
	ProviderRevisionID string
	ProviderKind       string
	UpstreamModel      string
	Timeout            time.Duration
	Priority           int
	Score              float64
	Strategy           string
	PolicyDigest       string
	VendorID           string
	Price              *usage.RoutingPrice
	Performance        *usage.Performance
}

// SelectionError names why no attempt could be planned.
type SelectionError struct{ Code string }

func (e *SelectionError) Error() string { return e.Code }

// Selection error codes.
const (
	RouteNotFound         = "route_not_found"
	OperationNotSupported = "operation_not_supported"
	NoEligibleTargets     = "no_eligible_targets"
)

// Select plans attempts for one request: enabled providers that certified the
// exact (model, operation, surface, mode), ordered by priority tier and then
// by weighted rendezvous score keyed on the affinity bytes.
func Select(s *Snapshot, slug, operation, surface, mode string, affinity []byte) ([]Attempt, error) {
	plan, err := PlanRequest(s, slug, operation, surface, mode, affinity, SelectionOptions{})
	if err != nil {
		return nil, err
	}
	if len(plan.Attempts) == 0 {
		return nil, &SelectionError{Code: NoEligibleTargets}
	}
	return plan.Attempts, nil
}

// Score is the weighted rendezvous score shared by live routing and
// simulation: weight / -ln(u), where u is a uniform sample from SHA-256 over
// the route, target, request tuple and affinity. Each variable-length input is
// length-prefixed so adjacent values cannot alias one another.
func Score(routeID, targetID uuid.UUID, weight int64, operation, surface, mode string, affinity []byte) float64 {
	h := sha256.New()
	h.Write([]byte("olp-weighted-rendezvous\x00"))
	h.Write(routeID[:])
	h.Write(targetID[:])
	var length [8]byte
	for _, input := range [][]byte{[]byte(operation), []byte(surface), []byte(mode), affinity} {
		binary.BigEndian.PutUint64(length[:], uint64(len(input)))
		h.Write(length[:])
		h.Write(input)
	}
	raw := binary.BigEndian.Uint64(h.Sum(nil)[:8])
	sample := (float64(raw>>11) + 1) / (float64(uint64(1)<<53) + 1)
	if weight < 1 {
		weight = 1
	}
	return float64(weight) / -math.Log(sample)
}

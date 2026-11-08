package routing

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
	// Discount is the multiplier pinned at plan time; accounting prices the
	// attempt under exactly it.
	Discount    *usage.RoutingDiscount
	Performance *usage.Performance
	// Slots, when set, preserve the plan's slot order and supply restrictions.
	// Nil leaves selection to execution, which rechecks credential eligibility.
	Slots []string
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

// Score is the weighted rendezvous score shared by live routing and
// simulation: weight / -ln(u), where u is a uniform sample from SHA-256 over
// the route, target, request tuple and affinity. Each variable-length input is
// length-prefixed so adjacent values cannot alias one another.
func Score(routeID, targetID uuid.UUID, weight int64, operation, surface, mode string, affinity []byte) float64 {
	var buffer [256]byte
	header := append(append(append(buffer[:0], "olp-weighted-rendezvous\x00"...), routeID[:]...), targetID[:]...)
	request := appendPrefixed(appendPrefixed(appendPrefixed(header, operation), surface), mode)
	var sum [sha256.Size]byte
	if len(affinity) <= len(buffer)-len(request)-8 {
		sum = sha256.Sum256(appendPrefixed(request, affinity))
	} else {
		h := sha256.New()
		h.Write(request)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(affinity)))
		h.Write(length[:])
		h.Write(affinity)
		copy(sum[:], h.Sum(buffer[:0]))
	}
	raw := binary.BigEndian.Uint64(sum[:8])
	sample := (float64(raw>>11) + 1) / (float64(uint64(1)<<53) + 1)
	effective := weight
	if effective < 1 {
		effective = 1
	}
	return float64(effective) / -math.Log(sample)
}

func appendPrefixed[T string | []byte](dst []byte, value T) []byte {
	return append(binary.BigEndian.AppendUint64(dst, uint64(len(value))), value...)
}

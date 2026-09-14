package runtime

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
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
	route, ok := s.Routes[slug]
	if !ok {
		return nil, &SelectionError{Code: RouteNotFound}
	}
	supported := false
	for _, declared := range route.Operations {
		supported = supported || declared == operation
	}
	if !supported {
		return nil, &SelectionError{Code: OperationNotSupported}
	}
	routeID, err := uuid.Parse(route.RoutingID)
	if err != nil {
		return nil, &SelectionError{Code: NoEligibleTargets}
	}
	var attempts []Attempt
	for _, target := range route.Targets {
		provider, ok := s.Providers[target.ProviderID]
		if !ok || !provider.Enabled || !provider.Supports(target.ProviderModel, operation, surface, mode) {
			continue
		}
		targetID, err := uuid.Parse(target.RoutingID)
		if err != nil {
			continue
		}
		attempts = append(attempts, Attempt{
			TargetID:           target.ID,
			ProviderID:         provider.ID,
			ProviderRevisionID: provider.RevisionID,
			ProviderKind:       provider.Kind,
			UpstreamModel:      target.ProviderModel,
			Timeout:            time.Duration(target.Timeout) * time.Millisecond,
			Priority:           target.Priority,
			Score:              Score(routeID, targetID, target.Weight, operation, surface, mode, affinity),
		})
	}
	if len(attempts) == 0 {
		return nil, &SelectionError{Code: NoEligibleTargets}
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].Priority != attempts[j].Priority {
			return attempts[i].Priority < attempts[j].Priority
		}
		return attempts[i].Score > attempts[j].Score
	})
	return attempts, nil
}

// Score is the weighted rendezvous score shared by live routing and
// simulation. It is byte-compatible with the reference gateway so existing
// affinity orderings carry over.
func Score(routeID, targetID uuid.UUID, weight int64, operation, surface, mode string, affinity []byte) float64 {
	h := sha256.New()
	h.Write([]byte("olp-v2-weighted-rendezvous\x00"))
	h.Write(routeID[:])
	h.Write(targetID[:])
	h.Write([]byte{operationTag(operation), surfaceTag(surface), modeTag(mode)})
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(affinity)))
	h.Write(length[:])
	h.Write(affinity)
	raw := binary.BigEndian.Uint64(h.Sum(nil)[:8])
	sample := (float64(raw>>11) + 1) / (float64(uint64(1)<<53) + 1)
	if weight < 1 {
		weight = 1
	}
	return float64(weight) / -math.Log(sample)
}

func operationTag(operation string) byte {
	switch operation {
	case "generation":
		return 0
	case "embeddings":
		return 1
	case "token_count":
		return 2
	case "image_generation":
		return 3
	case "image_edit":
		return 4
	case "image_variation":
		return 5
	case "speech":
		return 6
	case "transcription":
		return 7
	case "video_create":
		return 8
	case "video_list":
		return 9
	case "video_get":
		return 10
	case "video_content":
		return 11
	case "video_delete":
		return 12
	case "moderation":
		return 13
	case "model_list":
		return 14
	}
	return 15
}

func surfaceTag(surface string) byte {
	switch surface {
	case "anthropic":
		return 1
	case "gemini":
		return 2
	}
	return 0
}

func modeTag(mode string) byte {
	switch mode {
	case "streaming":
		return 1
	case "async":
		return 2
	}
	return 0
}

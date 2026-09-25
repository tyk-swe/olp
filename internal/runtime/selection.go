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
	RouteNotFound                  = "route_not_found"
	OperationNotSupported          = "operation_not_supported"
	NoEligibleTargets              = "no_eligible_targets"
	AttemptBudgetIncreaseForbidden = "attempt_budget_increase_forbidden"
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
// simulation. It is byte-compatible with the reference gateway so existing
// affinity orderings carry over.
func Score(routeID, targetID uuid.UUID, weight int64, operation, surface, mode string, affinity []byte) float64 {
	h := sha256.New()
	h.Write([]byte("olp-v2-weighted-rendezvous\x00"))
	h.Write(routeID[:])
	h.Write(targetID[:])
	opTag, surfaceID, modeID := operationTag(operation), surfaceTag(surface), modeTag(mode)
	h.Write([]byte{opTag, surfaceID, modeID})
	var length [8]byte
	// Keep the exact historical hash input for existing tuples. New registered
	// operations and native surfaces otherwise collide at their fallback tags.
	// Each extended dimension carries its own length so adjacent names cannot
	// alias one another or an existing affinity suffix.
	if !legacyScoreOperation(operation) || !legacyScoreSurface(surface) || !legacyScoreMode(mode) {
		h.Write([]byte{0xff})
		for _, dimension := range []struct {
			name   string
			legacy bool
		}{{operation, legacyScoreOperation(operation)}, {surface, legacyScoreSurface(surface)}, {mode, legacyScoreMode(mode)}} {
			if dimension.legacy {
				h.Write([]byte{0})
				continue
			}
			h.Write([]byte{1})
			binary.BigEndian.PutUint64(length[:], uint64(len(dimension.name)))
			h.Write(length[:])
			h.Write([]byte(dimension.name))
		}
	}
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

func legacyScoreOperation(operation string) bool {
	if operationTag(operation) != 15 {
		return true
	}
	// These original operations already used fallback tag 15. Preserve their
	// published affinity ordering while new trusted labels get distinct names.
	switch operation {
	case "rerank", "batch", "realtime", "bedrock_invoke":
		return true
	}
	return false
}

func legacyScoreSurface(surface string) bool {
	// Bedrock originally shared OpenAI's zero tag; both keep their old scores.
	return surface == "openai" || surface == "anthropic" || surface == "gemini" || surface == "bedrock"
}

func legacyScoreMode(mode string) bool {
	// Realtime originally shared unary's zero tag.
	return mode == "unary" || mode == "streaming" || mode == "async" || mode == "realtime"
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

package guardrailpolicy

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Evidence deliberately has no content, offsets, patterns, or replacements.
type Evidence struct {
	GuardrailID string   `json:"guardrail_id"`
	Revision    string   `json:"revision"`
	Phase       Phase    `json:"phase"`
	Action      Action   `json:"action"`
	Outcome     Outcome  `json:"outcome"`
	Categories  []string `json:"categories,omitempty"`
	LatencyMS   int64    `json:"latency_ms"`
}

func ValidateEvidence(values []Evidence) error {
	if len(values) > MaxEntries*4 {
		return errors.New("too many guardrail decisions")
	}
	for _, e := range values {
		if !identifier.MatchString(e.GuardrailID) || !identifier.MatchString(e.Revision) || !e.Phase.Valid() || !e.Action.Valid() || e.LatencyMS < 0 || e.LatencyMS > int64((24*time.Hour)/time.Millisecond) || len(e.Categories) > MaxCategories {
			return errors.New("invalid guardrail evidence")
		}
		if !e.Outcome.Valid() {
			return errors.New("invalid guardrail outcome")
		}
		for _, c := range e.Categories {
			if !categoryID.MatchString(c) {
				return errors.New("invalid guardrail category")
			}
		}
	}
	if len(values) > 0 {
		raw, _ := json.Marshal(values)
		if len(raw) > MaxEvidenceBytes {
			return errors.New("guardrail evidence exceeds byte bound")
		}
	}
	return nil
}

// EvidenceSize gives the conservative JSON size, including an array separator.
func EvidenceSize(value Evidence) int { raw, _ := json.Marshal(value); return len(raw) + 1 }

func MergeEvidence(decisions, values []Evidence) []Evidence {
	merged := slices.Clone(decisions)
	size := 2
	for _, d := range merged {
		size += EvidenceSize(d)
	}
	for _, v := range values {
		i := slices.IndexFunc(merged, func(d Evidence) bool {
			return d.GuardrailID == v.GuardrailID && d.Revision == v.Revision && d.Phase == v.Phase && d.Action == v.Action && d.Outcome == v.Outcome
		})
		if i < 0 {
			if len(merged) < MaxEntries*4 && size+EvidenceSize(v) <= MaxEvidenceBytes {
				merged = append(merged, v)
				size += EvidenceSize(v)
			}
			continue
		}
		next := merged[i]
		next.LatencyMS += v.LatencyMS
		next.Categories = slices.Compact(slices.Sorted(slices.Values(slices.Concat(next.Categories, v.Categories))))
		if grown := EvidenceSize(next) - EvidenceSize(merged[i]); len(next.Categories) <= MaxCategories && size+grown <= MaxEvidenceBytes {
			merged[i] = next
			size += grown
		}
	}
	return merged
}

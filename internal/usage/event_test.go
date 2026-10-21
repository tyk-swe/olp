package usage

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func metadataAttempt(ordinal int, providerID string, model string, committed bool) Attempt {
	completedAt := time.Now().UTC()
	usage := AttemptUsage{Observed: committed, Complete: true}
	if committed {
		tokens := int64(7)
		usage.InputTokens = &tokens
	}
	status := 200
	firstByte := int64(3)
	return Attempt{
		ID:            uuid.NewString(),
		Ordinal:       ordinal,
		ProviderID:    providerID,
		UpstreamModel: model,
		StartedAt:     completedAt.Add(-10 * time.Millisecond),
		CompletedAt:   completedAt,
		StatusCode:    &status,
		Committed:     committed,
		LatencyMS:     10,
		FirstByteMS:   &firstByte,
		Usage:         &usage,
	}
}

func metadataEvent() *Event {
	completedAt := time.Now().UTC()
	firstProvider := uuid.NewString()
	finalProvider := uuid.NewString()
	model := "model-b"
	status := 200
	firstByte := int64(18)
	tokens := int64(7)
	return &Event{
		Version:             WireVersion,
		EventID:             uuid.NewString(),
		RequestID:           uuid.NewString(),
		RuntimeGenerationID: uuid.NewString(),
		APIKeyID:            uuid.NewString(),
		ProviderID:          &finalProvider,
		RouteSlug:           "primary",
		UpstreamModel:       &model,
		Operation:           "generation",
		Surface:             "openai",
		RequestStartedAt:    completedAt.Add(-25 * time.Millisecond),
		RequestCompletedAt:  completedAt,
		ObservedAt:          completedAt,
		StatusCode:          &status,
		Committed:           true,
		LatencyMS:           25,
		FirstByteMS:         &firstByte,
		InputTokens:         &tokens,
		UsageComplete:       true,
		Attempts: []Attempt{
			metadataAttempt(1, firstProvider, "model-a", false),
			metadataAttempt(2, finalProvider, "model-b", true),
		},
	}
}

func TestValidateRejectsMalformedEnvelopesAndAttempts(t *testing.T) {
	negative := "-0.000001"
	cases := []struct {
		name   string
		mutate func(*Event)
	}{
		{"reversed request timing", func(e *Event) {
			e.RequestCompletedAt = e.RequestStartedAt.Add(-time.Nanosecond)
		}},
		{"blank route", func(e *Event) { e.RouteSlug = "  " }},
		{"request status outside HTTP range", func(e *Event) {
			status := 99
			e.StatusCode = &status
		}},
		{"final provider mismatch", func(e *Event) {
			other := uuid.NewString()
			e.ProviderID = &other
		}},
		{"final model mismatch", func(e *Event) {
			other := "model-z"
			e.UpstreamModel = &other
		}},
		{"final commit mismatch", func(e *Event) { e.Committed = false }},
		{"target metadata without an attempt", func(e *Event) { e.Attempts = nil }},
		{"missing attempt-local usage", func(e *Event) { e.Attempts[0].Usage = nil }},
		{"non-contiguous ordinal", func(e *Event) { e.Attempts[0].Ordinal = 2 }},
		{"reversed attempt timing", func(e *Event) {
			e.Attempts[0].CompletedAt = e.Attempts[0].StartedAt.Add(-time.Nanosecond)
		}},
		{"attempt status outside HTTP range", func(e *Event) {
			status := 600
			e.Attempts[0].StatusCode = &status
		}},
		{"request latency overflow", func(e *Event) { e.LatencyMS = math.MaxInt32 + 1 }},
		{"request first-byte overflow", func(e *Event) {
			overflow := int64(math.MaxInt32) + 1
			e.FirstByteMS = &overflow
		}},
		{"attempt latency overflow", func(e *Event) { e.Attempts[0].LatencyMS = math.MaxInt32 + 1 }},
		{"attempt first-byte overflow", func(e *Event) {
			overflow := int64(math.MaxInt32) + 1
			e.Attempts[0].FirstByteMS = &overflow
		}},
		{"missing evidence without billing uncertainty", func(e *Event) {
			e.Attempts[0].Usage.Complete = false
		}},
		{"complete and billing-uncertain", func(e *Event) {
			e.Attempts[0].Usage.BillingUncertain = true
			e.Attempts[0].Usage.Complete = true
		}},
		{"observed and billing-uncertain", func(e *Event) {
			e.Attempts[1].Usage.BillingUncertain = true
		}},
		{"tokens without observed usage", func(e *Event) {
			tokens := int64(1)
			e.Attempts[0].Usage.InputTokens = &tokens
		}},
		{"media units without observed usage", func(e *Event) {
			units := "1.5"
			e.Attempts[0].Usage.MediaUnits = &units
		}},
		{"negative token count", func(e *Event) {
			tokens := int64(-1)
			e.Attempts[1].Usage.OutputTokens = &tokens
		}},
		{"negative media units", func(e *Event) { e.Attempts[1].Usage.MediaUnits = &negative }},
		{"attempt 5m cache detail without a cache write total", func(e *Event) {
			zero := int64(0)
			e.Attempts[1].Usage.CacheWrite5MInputTokens = &zero
		}},
		{"attempt 1h cache detail without a cache write total", func(e *Event) {
			zero := int64(0)
			e.Attempts[1].Usage.CacheWrite1HInputTokens = &zero
		}},
		{"request cache detail without a cache write total", func(e *Event) {
			zero := int64(0)
			e.CacheWrite5MInputTokens = &zero
		}},
		{"attempt cache reads and writes overflowing the input total", func(e *Event) {
			huge := int64(math.MaxInt64)
			e.Attempts[1].Usage.CachedInputTokens = &huge
			e.Attempts[1].Usage.CacheWriteInputTokens = &huge
		}},
		{"attempt cache write detail overflowing the write total", func(e *Event) {
			total, huge := int64(5), int64(math.MaxInt64)
			e.Attempts[1].Usage.CacheWriteInputTokens = &total
			e.Attempts[1].Usage.CacheWrite5MInputTokens = &huge
			e.Attempts[1].Usage.CacheWrite1HInputTokens = &huge
		}},
		{"request cache reads and writes overflowing the input total", func(e *Event) {
			huge := int64(math.MaxInt64)
			e.CachedInputTokens = &huge
			e.CacheWriteInputTokens = &huge
		}},
		{"malformed media units", func(e *Event) {
			units := "1.2.3"
			e.Attempts[1].Usage.MediaUnits = &units
		}},
		{"negative input estimate", func(e *Event) {
			estimated := int64(-1)
			e.Attempts[1].EstimatedInputTokens, e.Attempts[1].EstimateProvenance = &estimated, EstimateTokenizer
		}},
		{"input estimate without provenance", func(e *Event) {
			estimated := int64(12)
			e.Attempts[1].EstimatedInputTokens = &estimated
		}},
		{"estimate provenance without an input estimate", func(e *Event) {
			e.Attempts[1].EstimateProvenance = EstimateHeuristic
		}},
		{"unknown estimate provenance", func(e *Event) {
			estimated := int64(12)
			e.Attempts[1].EstimatedInputTokens, e.Attempts[1].EstimateProvenance = &estimated, "guess"
		}},
		{"malformed model family", func(e *Event) { e.Attempts[1].ModelFamily = "OpenAI o200k" }},
		{"oversized model family", func(e *Event) { e.Attempts[1].ModelFamily = "f" + strings.Repeat("a", 64) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := metadataEvent()
			tc.mutate(event)
			validated, err := Validate(event)
			if validated != nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("err = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

// The first attempt never carried routing or usage evidence, so neither key
// is written at all.

func TestDecodeRejectsMalformedFields(t *testing.T) {
	base := metadataEvent()
	payload, err := Encode(base)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]json.RawMessage)
	}{
		{"missing event id", func(m map[string]json.RawMessage) { delete(m, "event_id") }},
		{"null event id", func(m map[string]json.RawMessage) { m["event_id"] = json.RawMessage(`null`) }},
		{"event id is not a uuid", func(m map[string]json.RawMessage) {
			m["event_id"] = json.RawMessage(`"not-a-uuid"`)
		}},
		{"missing attempts", func(m map[string]json.RawMessage) { delete(m, "attempts") }},
		{"null attempts", func(m map[string]json.RawMessage) { m["attempts"] = json.RawMessage(`null`) }},
		{"missing committed", func(m map[string]json.RawMessage) { delete(m, "committed") }},
		{"missing route slug", func(m map[string]json.RawMessage) { delete(m, "route_slug") }},
		{"unknown operation", func(m map[string]json.RawMessage) {
			m["operation"] = json.RawMessage(`"chat"`)
		}},
		{"unknown surface", func(m map[string]json.RawMessage) {
			m["surface"] = json.RawMessage(`"vertex"`)
		}},
		{"negative latency", func(m map[string]json.RawMessage) {
			m["latency_ms"] = json.RawMessage(`-1`)
		}},
		{"fractional latency", func(m map[string]json.RawMessage) {
			m["latency_ms"] = json.RawMessage(`1.5`)
		}},
		{"negative status", func(m map[string]json.RawMessage) {
			m["status_code"] = json.RawMessage(`-200`)
		}},
		{"status above the wire width", func(m map[string]json.RawMessage) {
			m["status_code"] = json.RawMessage(`70000`)
		}},
		{"timestamp is not rfc3339", func(m map[string]json.RawMessage) {
			m["observed_at"] = json.RawMessage(`"yesterday"`)
		}},
		{"media units is a number", func(m map[string]json.RawMessage) {
			m["media_units"] = json.RawMessage(`1.5`)
		}},
		{"media units is not a decimal", func(m map[string]json.RawMessage) {
			m["media_units"] = json.RawMessage(`"1.5e3"`)
		}},
		{"media units exceed the stored precision", func(m map[string]json.RawMessage) {
			m["media_units"] = json.RawMessage(`"1234567890123456789"`)
		}},
		{"attempt is missing its ordinal", func(m map[string]json.RawMessage) {
			m["attempts"] = json.RawMessage(`[{"id":"` + uuid.NewString() + `"}]`)
		}},
		{"attempt usage is missing a flag", func(m map[string]json.RawMessage) {
			var attempts []map[string]json.RawMessage
			if err := json.Unmarshal(m["attempts"], &attempts); err != nil {
				t.Fatalf("unmarshal attempts: %v", err)
			}
			attempts[0]["usage"] = json.RawMessage(`{"observed":true,"complete":true}`)
			m["attempts"] = marshalForTest(t, attempts)
		}},
		{"routing without a provider revision", func(m map[string]json.RawMessage) {
			var attempts []map[string]json.RawMessage
			if err := json.Unmarshal(m["attempts"], &attempts); err != nil {
				t.Fatalf("unmarshal attempts: %v", err)
			}
			attempts[0]["routing"] = json.RawMessage(`{"mode":"unary"}`)
			m["attempts"] = marshalForTest(t, attempts)
		}},
		{"estimate is a string", withAttemptFields(t, map[string]string{
			"estimated_input_tokens": `"12"`, "estimate_provenance": `"tokenizer"`})},
		{"estimate is fractional", withAttemptFields(t, map[string]string{
			"estimated_input_tokens": `1.5`, "estimate_provenance": `"tokenizer"`})},
		{"estimate is negative", withAttemptFields(t, map[string]string{
			"estimated_input_tokens": `-1`, "estimate_provenance": `"tokenizer"`})},
		{"estimate without provenance", withAttemptFields(t, map[string]string{
			"estimated_input_tokens": `12`})},
		{"provenance without estimate", withAttemptFields(t, map[string]string{
			"estimate_provenance": `"heuristic"`})},
		{"provenance is unknown", withAttemptFields(t, map[string]string{
			"estimated_input_tokens": `12`, "estimate_provenance": `"guess"`})},
		{"provenance is a number", withAttemptFields(t, map[string]string{
			"estimated_input_tokens": `12`, "estimate_provenance": `1`})},
		{"model family is malformed", withAttemptFields(t, map[string]string{
			"model_family": `"Not A Family"`})},
		{"model family is a number", withAttemptFields(t, map[string]string{
			"model_family": `7`})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := make(map[string]json.RawMessage, len(envelope))
			for key, value := range envelope {
				candidate[key] = value
			}
			tc.mutate(candidate)
			event, err := Decode(marshalForTest(t, candidate))
			if err == nil {
				t.Fatalf("accepted %s (event %+v)", tc.name, event)
			}
			if errors.Is(err, ErrInvalidEvent) {
				t.Error("a malformed payload must not read as an invalid event")
			}
		})
	}
}

// withAttemptFields sets raw JSON fields on the first attempt of an envelope.
func withAttemptFields(t *testing.T, fields map[string]string) func(map[string]json.RawMessage) {
	t.Helper()
	return func(m map[string]json.RawMessage) {
		var attempts []map[string]json.RawMessage
		if err := json.Unmarshal(m["attempts"], &attempts); err != nil {
			t.Fatalf("unmarshal attempts: %v", err)
		}
		for key, value := range fields {
			attempts[0][key] = json.RawMessage(value)
		}
		m["attempts"] = marshalForTest(t, attempts)
	}
}

func marshalForTest(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

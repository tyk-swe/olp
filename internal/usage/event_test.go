package usage

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
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

func TestValidateNormalizesAWellFormedAttemptSequence(t *testing.T) {
	event := metadataEvent()

	validated, err := Validate(event)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !validated.HasAttempts {
		t.Error("expected attempts")
	}
	if validated.StatusCode == nil || *validated.StatusCode != 200 {
		t.Errorf("status = %v, want 200", validated.StatusCode)
	}
	if validated.LatencyMS != 25 {
		t.Errorf("latency = %d, want 25", validated.LatencyMS)
	}
	if validated.FirstByteMS == nil || *validated.FirstByteMS != 18 {
		t.Errorf("first byte = %v, want 18", validated.FirstByteMS)
	}
	if validated.AttemptCount != 2 || len(validated.Attempts) != 2 {
		t.Fatalf("attempt count = %d, want 2", validated.AttemptCount)
	}
	if validated.Attempts[0].Ordinal != 1 {
		t.Errorf("first ordinal = %d, want 1", validated.Attempts[0].Ordinal)
	}
	final := validated.Attempts[1]
	if final.StatusCode == nil || *final.StatusCode != 200 {
		t.Errorf("attempt status = %v, want 200", final.StatusCode)
	}
	if final.LatencyMS != 10 {
		t.Errorf("attempt latency = %d, want 10", final.LatencyMS)
	}
	if final.FirstByteMS == nil || *final.FirstByteMS != 3 {
		t.Errorf("attempt first byte = %v, want 3", final.FirstByteMS)
	}
	if final.Attempt != &event.Attempts[1] {
		t.Error("validated attempt does not point at the event's attempt")
	}
	if !final.Usage.Observed || final.Usage.InputTokens == nil || *final.Usage.InputTokens != 7 {
		t.Errorf("attempt usage = %+v, want observed with 7 input tokens", final.Usage)
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
		{"malformed media units", func(e *Event) {
			units := "1.2.3"
			e.Attempts[1].Usage.MediaUnits = &units
		}},
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

func TestValidateAcceptsTheAttemptUsageStates(t *testing.T) {
	cases := []struct {
		name  string
		usage AttemptUsage
	}{
		{"settled without evidence", AttemptUsage{Complete: true}},
		{"billing uncertain", AttemptUsage{BillingUncertain: true}},
		{"observed and settled", AttemptUsage{Observed: true, Complete: true}},
		{"observed but not settled", AttemptUsage{Observed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := metadataEvent()
			usage := tc.usage
			event.Attempts[1].Usage = &usage
			if _, err := Validate(event); err != nil {
				t.Fatalf("rejected %s: %v", tc.name, err)
			}
		})
	}
}

func TestValidateAcceptsEmptyPreAttemptEventsOnlyWithoutTargetOrUsage(t *testing.T) {
	event := metadataEvent()
	event.Attempts = nil
	event.ProviderID = nil
	event.UpstreamModel = nil
	event.Committed = false
	event.FirstByteMS = nil
	event.InputTokens = nil
	event.OutputTokens = nil
	event.CachedInputTokens = nil
	event.MediaUnits = nil
	event.UsageComplete = false

	validated, err := Validate(event)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if validated.HasAttempts || validated.AttemptCount != 0 || len(validated.Attempts) != 0 {
		t.Fatalf("validated = %+v, want an empty attempt sequence", validated)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Event)
	}{
		{"target provider", func(e *Event) { id := uuid.NewString(); e.ProviderID = &id }},
		{"target model", func(e *Event) { model := "model-a"; e.UpstreamModel = &model }},
		{"committed", func(e *Event) { e.Committed = true }},
		{"first byte", func(e *Event) { ms := int64(3); e.FirstByteMS = &ms }},
		{"input tokens", func(e *Event) { tokens := int64(1); e.InputTokens = &tokens }},
		{"output tokens", func(e *Event) { tokens := int64(1); e.OutputTokens = &tokens }},
		{"cached input tokens", func(e *Event) { tokens := int64(1); e.CachedInputTokens = &tokens }},
		{"media units", func(e *Event) { units := "1.5"; e.MediaUnits = &units }},
		{"usage complete", func(e *Event) { e.UsageComplete = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := metadataEvent()
			candidate.Attempts = nil
			candidate.ProviderID = nil
			candidate.UpstreamModel = nil
			candidate.Committed = false
			candidate.FirstByteMS = nil
			candidate.InputTokens = nil
			candidate.UsageComplete = false
			tc.mutate(candidate)
			if _, err := Validate(candidate); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("err = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

func TestDecodeRejectsPayloadsWithoutTheCurrentVersion(t *testing.T) {
	payload, err := Encode(metadataEvent())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	withVersion := func(version string) string {
		changed := maps.Clone(envelope)
		if version == "" {
			delete(changed, "version")
		} else {
			changed["version"] = json.RawMessage(version)
		}
		return string(marshalForTest(t, changed))
	}
	cases := []struct {
		name    string
		payload string
	}{
		{"missing version", withVersion("")},
		{"null version", withVersion("null")},
		{"other version", withVersion("2")},
		{"zero version", withVersion("0")},
		{"non-numeric version", withVersion(`"1"`)},
		{"negative version", withVersion("-1")},
		{"current version without fields", `{"version":1,"future_optional_field":true}`},
		{"invalid json", `invalid json`},
		{"json array", `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, err := Decode([]byte(tc.payload))
			if err == nil {
				t.Fatal("decoded a payload without the current version and contract")
			}
			if event != nil {
				t.Fatalf("event = %+v, want none", event)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	event := metadataEvent()
	units := "12.500000"
	event.Attempts[1].Usage.MediaUnits = &units
	policy := json.RawMessage(`{"strategy":"failover"}`)
	mode := "streaming"
	firstOutput := int64(9)
	streamed := int64(11)
	slot := uuid.NewString()
	version := uuid.NewString()
	pricing := uuid.NewString()
	event.Attempts[1].Routing = &Routing{
		Policy:               &policy,
		Mode:                 &mode,
		FirstOutputMS:        &firstOutput,
		StreamedOutputTokens: &streamed,
		CredentialSlotID:     &slot,
		CredentialVersionID:  &version,
		ProviderRevisionID:   uuid.NewString(),
		PricingRevisionID:    &pricing,
	}
	event.Version = 0

	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if event.Version != 0 || event.Attempts == nil {
		t.Error("encode mutated the caller's event")
	}
	decoded, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	event.Version = WireVersion
	if !decodedMatches(t, event, decoded) {
		t.Errorf("round trip changed the event:\n got %s", payload)
	}
	if _, err = Validate(decoded); err != nil {
		t.Fatalf("validate round trip: %v", err)
	}
}

func decodedMatches(t *testing.T, want *Event, got *Event) bool {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	return string(wantJSON) == string(gotJSON)
}

func TestEncodePreAttemptEventCarriesAnEmptyAttemptList(t *testing.T) {
	event := metadataEvent()
	event.Attempts = nil
	event.ProviderID = nil
	event.UpstreamModel = nil
	event.Committed = false
	event.FirstByteMS = nil
	event.InputTokens = nil
	event.UsageComplete = false

	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(payload), `"attempts":[]`) {
		t.Errorf("payload = %s, want an empty attempt list", payload)
	}
	decoded, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Attempts) != 0 {
		t.Errorf("attempts = %d, want none", len(decoded.Attempts))
	}
}

func TestSerializedEventCarriesNoContentFields(t *testing.T) {
	event := metadataEvent()
	event.Attempts[1].Routing = &Routing{ProviderRevisionID: uuid.NewString()}
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantEvent := []string{
		"api_key_id", "attempts", "budget_group_id", "cache_write_1h_input_tokens",
		"cache_write_5m_input_tokens", "cache_write_input_tokens", "cached_input_tokens",
		"committed", "error_class", "event_id", "first_byte_ms", "input_tokens", "latency_ms",
		"media_units", "observed_at", "operation", "output_tokens", "provider_id",
		"request_completed_at", "request_id", "request_started_at", "route_slug",
		"runtime_generation_id", "status_code", "surface", "unpriced", "upstream_model",
		"usage_complete", "version",
	}
	assertJSONKeys(t, "event", envelope, wantEvent)

	var attempts []map[string]json.RawMessage
	if err = json.Unmarshal(envelope["attempts"], &attempts); err != nil {
		t.Fatalf("unmarshal attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	assertJSONKeys(t, "attempt", attempts[1], []string{
		"committed", "completed_at", "error_class", "first_byte_ms", "id", "latency_ms",
		"ordinal", "provider_id", "routing", "started_at", "status_code", "upstream_model", "usage",
	})
	var usage map[string]json.RawMessage
	if err = json.Unmarshal(attempts[1]["usage"], &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	assertJSONKeys(t, "usage", usage, []string{
		"billing_uncertain", "cache_write_1h_input_tokens", "cache_write_5m_input_tokens",
		"cache_write_input_tokens", "cached_input_tokens", "complete", "input_tokens",
		"media_units", "observed", "output_tokens",
	})
	var routing map[string]json.RawMessage
	if err = json.Unmarshal(attempts[1]["routing"], &routing); err != nil {
		t.Fatalf("unmarshal routing: %v", err)
	}
	assertJSONKeys(t, "routing", routing, []string{
		"credential_slot_id", "credential_version_id", "pricing_revision_id", "provider_revision_id",
	})
	// The first attempt never carried routing or usage evidence, so neither key
	// is written at all.
	assertJSONKeys(t, "pre-routing attempt", attempts[0], []string{
		"committed", "completed_at", "error_class", "first_byte_ms", "id", "latency_ms",
		"ordinal", "provider_id", "started_at", "status_code", "upstream_model", "usage",
	})
}

func assertJSONKeys(t *testing.T, label string, object map[string]json.RawMessage, want []string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s keys = %v, want %v", label, got, want)
	}
}

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

func marshalForTest(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func TestParseDecimalBoundsAndSign(t *testing.T) {
	cases := []struct {
		value    string
		negative bool
		wantErr  bool
	}{
		{value: "0"},
		{value: "0.000000"},
		{value: "-0.000000"},
		{value: "12.500000"},
		{value: "+1.5"},
		{value: "-0.000001", negative: true},
		{value: "-12", negative: true},
		{value: "123456789012345678.123456"},
		{value: "", wantErr: true},
		{value: "-", wantErr: true},
		{value: ".", wantErr: true},
		{value: ".5", wantErr: true},
		{value: "5.", wantErr: true},
		{value: "1.2.3", wantErr: true},
		{value: "1e3", wantErr: true},
		{value: " 1", wantErr: true},
		{value: "1234567890123456789", wantErr: true},
		{value: "1.1234567", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			negative, err := parseWireDecimal(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil && negative != tc.negative {
				t.Errorf("negative = %v, want %v", negative, tc.negative)
			}
		})
	}
}

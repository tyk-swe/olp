package usage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func metadataAttempt(ordinal int, providerID string, model string, committed bool) Attempt {
	completedAt := time.Now().UTC()
	usage := AttemptUsage{Evidence: EvidenceFrom(committed, true, false)}
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

const (
	populatedEventBytes = `{"training_tokens":5,"storage_byte_seconds":"12.5","cache_token_seconds":"7.25","container_sessions":{"default":2},"cache_result":"miss","evaluator_guardrail_id":"44444444-4444-4444-8444-444444444444","budget_boundary":"project","version":1,"event_id":"11111111-1111-4111-8111-111111111111","request_id":"22222222-2222-4222-8222-222222222222","runtime_generation_id":"33333333-3333-4333-8333-333333333333","end_user_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","api_key_id":"55555555-5555-4555-8555-555555555555","agent_principal_id":"66666666-6666-4666-8666-666666666666","member_principal_id":"77777777-7777-4777-8777-777777777777","delegation_pair_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","attribution":{"team":"core"},"origin":"classifier","parent_request_id":"88888888-8888-4888-8888-888888888888","guardrail_decisions":[{"guardrail_id":"g1","revision":"r1","phase":"input","action":"annotate","outcome":"annotated","categories":["cat.a"],"latency_ms":4}],"policy_decisions":[{"rule_id":"rule-a","phase":"input","action":"block","outcome":"blocked"}],"budget_group_id":"99999999-9999-4999-8999-999999999999","provider_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","route_slug":"primary","upstream_model":"model-b","operation":"generation","surface":"openai","request_started_at":"2026-01-02T03:04:05.123456789Z","request_completed_at":"2026-01-02T03:04:06Z","observed_at":"2026-01-02T03:04:06Z","status_code":402,"error_class":"budget_exhausted","committed":true,"latency_ms":25,"first_byte_ms":18,"input_tokens":100,"output_tokens":50,"cached_input_tokens":10,"cache_write_input_tokens":6,"cache_write_5m_input_tokens":2,"cache_write_1h_input_tokens":1,"audio_input_tokens":4,"cached_audio_input_tokens":1,"audio_output_tokens":3,"image_input_tokens":2,"cached_image_input_tokens":1,"image_output_tokens":2,"media_units":"1.5","tool_calls":{"web_search":2},"batch":true,"usage_complete":true,"unpriced":true,"payload_captured":true,"attempts":[{"execution":{"dispatch_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","run_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","step_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","target_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","route_revision_id":"ffffffff-ffff-4fff-8fff-ffffffffffff"},"guardrail_decisions":[{"guardrail_id":"g2","revision":"r2","phase":"input","action":"annotate","outcome":"annotated","latency_ms":0}],"routing":{"prompt":{"id":"01010101-0101-4101-8101-010101010101","revision_id":"02020202-0202-4202-8202-020202020202"},"cache":{"result":"hit","currency":"USD","age_seconds":3,"similarity":0.5,"avoided_cost":"1.5","budget_cost":"0.5","budget_charge":"0.25"},"credential_source":"operator","interaction":{"fidelity":"strict","plan_class":"native_identity","upstream_state":"accepted","client_state":"terminal"},"policy":{"kept":["raw"]},"mode":"auto","first_output_ms":9,"streamed_output_tokens":8,"credential_slot_id":"03030303-0303-4303-8303-030303030303","credential_version_id":"04040404-0404-4404-8404-040404040404","provider_revision_id":"05050505-0505-4505-8505-050505050505","pricing_revision_id":"06060606-0606-4606-8606-060606060606","leg":{"route":"fallback-route","revision_id":"07070707-0707-4707-8707-070707070707","via":"fallback"},"selector":"sel-1","retry":1,"budgets":["08080808-0808-4808-8808-080808080808"],"baseline":{"provider_id":"09090909-0909-4909-8909-090909090909","upstream_model":"base","vendor_id":"openai"}},"id":"0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a","ordinal":1,"provider_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","upstream_model":"model-b","started_at":"2026-01-02T03:04:05Z","completed_at":"2026-01-02T03:04:06Z","status_code":402,"error_class":"budget_exhausted","committed":true,"latency_ms":20,"first_byte_ms":15,"usage":{"invocation_count":3,"training_tokens":4,"storage_byte_seconds":"8.5","cache_token_seconds":"2.25","container_sessions":{"1g":1},"deferred":true,"observed":true,"complete":false,"billing_uncertain":true,"input_tokens":100,"output_tokens":50,"cached_input_tokens":10,"cache_write_input_tokens":6,"cache_write_5m_input_tokens":2,"cache_write_1h_input_tokens":1,"audio_input_tokens":4,"cached_audio_input_tokens":1,"audio_output_tokens":3,"image_input_tokens":2,"cached_image_input_tokens":1,"image_output_tokens":2,"media_units":"1.5","tool_calls":{"web_search":2},"batch":true},"estimated_input_tokens":0,"estimate_provenance":"tokenizer","model_family":"gpt-4"}]}`
	minimalEventBytes   = `{"version":1,"event_id":"11111111-1111-4111-8111-111111111111","request_id":"22222222-2222-4222-8222-222222222222","runtime_generation_id":"33333333-3333-4333-8333-333333333333","api_key_id":"","origin":"probe","budget_group_id":null,"provider_id":null,"route_slug":"primary","upstream_model":null,"operation":"generation","surface":"unknown","request_started_at":"2026-01-02T03:04:05.123456789Z","request_completed_at":"2026-01-02T03:04:06Z","observed_at":"2026-01-02T03:04:06Z","status_code":null,"error_class":null,"committed":false,"latency_ms":0,"first_byte_ms":null,"input_tokens":null,"output_tokens":null,"cached_input_tokens":null,"cache_write_input_tokens":null,"cache_write_5m_input_tokens":null,"cache_write_1h_input_tokens":null,"audio_input_tokens":null,"cached_audio_input_tokens":null,"audio_output_tokens":null,"image_input_tokens":null,"cached_image_input_tokens":null,"image_output_tokens":null,"media_units":null,"usage_complete":false,"unpriced":false,"payload_captured":false,"attempts":[]}`
)

func TestEncodedEventBytesAreStable(t *testing.T) {
	for name, want := range map[string]string{"populated": populatedEventBytes, "minimal": minimalEventBytes} {
		t.Run(name, func(t *testing.T) {
			event, err := Decode([]byte(want))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Validate(event); err != nil {
				t.Fatal(err)
			}
			got, err := Encode(event)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatalf("encoded bytes changed\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestHalfAnInputEstimateIsRejectedOnTheWire(t *testing.T) {
	for name, field := range map[string]string{
		"tokens without provenance": `"estimate_provenance":"tokenizer",`,
		"provenance without tokens": `"estimated_input_tokens":0,`,
	} {
		t.Run(name, func(t *testing.T) {
			encoded := strings.Replace(populatedEventBytes, field, "", 1)
			if encoded == populatedEventBytes {
				t.Fatal("fixture no longer carries the field")
			}
			if _, err := Decode([]byte(encoded)); err == nil {
				t.Fatal("half an input estimate was accepted")
			}
		})
	}
}

func TestInconsistentWireEvidenceIsRejected(t *testing.T) {
	const encoded = `"observed":true,"complete":false,"billing_uncertain":true`
	for _, flags := range []string{
		`"observed":false,"complete":false,"billing_uncertain":false`,
		`"observed":false,"complete":true,"billing_uncertain":true`,
		`"observed":true,"complete":true,"billing_uncertain":true`,
	} {
		event, err := Decode([]byte(strings.Replace(populatedEventBytes, encoded, flags, 1)))
		if err == nil {
			_, err = Validate(event)
		}
		if err == nil {
			t.Fatalf("accepted %s", flags)
		}
	}
}

func TestEndUserDigestsAgreeWithAccessPolicy(t *testing.T) {
	for _, tc := range []struct {
		digest string
		valid  bool
	}{
		{strings.Repeat("a", 64), true}, {strings.Repeat("0", 64), true},
		{strings.Repeat("A", 64), false}, {strings.Repeat("a", 63), false},
		{strings.Repeat("a", 65), false}, {strings.Repeat("g", 64), false},
	} {
		event := metadataEvent()
		event.EndUserDigest = tc.digest
		_, validationErr := Validate(event)
		payload, err := Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		decoded, decodeErr := Decode(payload)
		if keyauthority.ValidEndUserDigest(tc.digest) != tc.valid || (validationErr == nil) != tc.valid || (decodeErr == nil) != tc.valid {
			t.Errorf("digest %q, access valid %v, validation error %v, decode error %v", tc.digest, keyauthority.ValidEndUserDigest(tc.digest), validationErr, decodeErr)
		}
		if decodeErr == nil && decoded.EndUserDigest != tc.digest {
			t.Errorf("digest %q decoded as %q", tc.digest, decoded.EndUserDigest)
		}
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
		{"inconsistent usage evidence", func(e *Event) {
			e.Attempts[0].Usage.Evidence = EvidenceUnset
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
			e.Attempts[1].InputEstimate = &InputEstimate{Tokens: -1, Provenance: EstimateTokenizer}
		}},
		{"input estimate without provenance", func(e *Event) {
			e.Attempts[1].InputEstimate = &InputEstimate{Tokens: 12}
		}},
		{"unknown estimate provenance", func(e *Event) {
			e.Attempts[1].InputEstimate = &InputEstimate{Tokens: 12, Provenance: "guess"}
		}},
		{"malformed model family", func(e *Event) { e.Attempts[1].ModelFamily = "OpenAI o200k" }},
		{"oversized model family", func(e *Event) { e.Attempts[1].ModelFamily = "f" + strings.Repeat("a", 64) }},
		{"malformed request guardrail evidence", func(e *Event) {
			e.GuardrailDecisions = []guardrails.Evidence{{GuardrailID: "g", Revision: "r", Phase: guardrails.Input, Action: guardrails.Block, Outcome: "maybe"}}
		}},
		{"malformed attempt guardrail evidence", func(e *Event) {
			e.Attempts[1].GuardrailDecisions = []guardrails.Evidence{{GuardrailID: "g", Revision: "r", Phase: guardrails.Input, Action: guardrails.Block, Outcome: "maybe"}}
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

func TestRouteSlugsAgreeAcrossRequestsAccessAccountingAndCache(t *testing.T) {
	for _, tc := range []struct {
		slug  string
		valid bool
	}{
		{"route", true}, {"route-a.b_c", true}, {"0route", true}, {"UPPER", false}, {"route/path", false},
		{"-route", false}, {"route\n", false}, {"røute", false}, {"", false},
		{strings.Repeat("a", 100), true}, {strings.Repeat("a", 101), false},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			event := metadataEvent()
			event.Attempts[0].Routing = &Routing{
				ProviderRevisionID: uuid.NewString(),
				Leg:                &RouteLeg{Route: tc.slug, RevisionID: uuid.NewString(), Via: ViaSelector},
			}
			payload, err := Encode(event)
			if err != nil {
				t.Fatal(err)
			}
			_, accountingErr := Decode(payload)
			request, err := json.Marshal(map[string]any{"model": tc.slug, "messages": []map[string]string{{"role": "user", "content": "hello"}}})
			if err != nil {
				t.Fatal(err)
			}
			_, requestErr := openai.Parse(wire.Chat, request)
			accessErr := (keyauthority.RouteGroups{"group": []string{tc.slug}}).Validate()
			cacheErr := (&cache.Policy{Mode: "semantic", EmbeddingRoute: tc.slug, AllowUnsealedVectors: true}).Validate()
			if (accountingErr == nil) != tc.valid || (requestErr == nil) != tc.valid || (accessErr == nil) != tc.valid || (cacheErr == nil) != tc.valid {
				t.Fatalf("accounting error = %v, request error = %v, access error = %v, cache error = %v", accountingErr, requestErr, accessErr, cacheErr)
			}
		})
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
		{"unknown budget boundary", func(m map[string]json.RawMessage) {
			m["error_class"] = json.RawMessage(`"budget_exhausted"`)
			m["budget_boundary"] = json.RawMessage(`"unknown"`)
		}},
		{"budget boundary without budget refusal", func(m map[string]json.RawMessage) {
			m["budget_boundary"] = json.RawMessage(`"api_key"`)
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

func TestBudgetBoundarySurvivesUsageEvent(t *testing.T) {
	now := time.Now().UTC()
	status := 429
	code := "budget_exhausted"
	for _, boundary := range []string{"api_key", "budget_group", "key_route", "key_end_user", "project_end_user", "attribution", "project", "organization", "installation"} {
		t.Run(boundary, func(t *testing.T) {
			event := &Event{
				EventID: uuid.NewString(), RequestID: uuid.NewString(), RuntimeGenerationID: uuid.NewString(), APIKeyID: uuid.NewString(),
				RouteSlug: "primary", Operation: "generation", Surface: "openai",
				RequestStartedAt: now, RequestCompletedAt: now, ObservedAt: now,
				StatusCode: &status, ErrorClass: &code, BudgetBoundary: boundary,
			}
			payload, err := Encode(event)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			decoded, err := Decode(payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, err := Validate(decoded); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if decoded.BudgetBoundary != boundary {
				t.Fatalf("budget boundary = %q, want %q", decoded.BudgetBoundary, boundary)
			}
		})
	}
}

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

func TestDecodeAcceptsPassthroughOperation(t *testing.T) {
	event := metadataEvent()
	event.Operation = "passthrough"
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode passthrough: %v", err)
	}
	decoded, err := Decode(payload)
	if err != nil || decoded.Operation != "passthrough" {
		t.Fatalf("decode passthrough: event=%v err=%v", decoded, err)
	}
	if !ValidOperation("passthrough") {
		t.Fatal("pricing vocabulary rejects passthrough")
	}
}

func TestDecodeAcceptsRetainedOperations(t *testing.T) {
	for _, operation := range []string{"fine_tuning", "vector_store", "retrieval", "conversation", "container", "eval", "skill", "context_cache"} {
		event := metadataEvent()
		event.Operation = operation
		payload, err := Encode(event)
		if err != nil {
			t.Fatalf("encode %s: %v", operation, err)
		}
		decoded, err := Decode(payload)
		if err != nil || decoded.Operation != operation {
			t.Fatalf("decode %s: event=%v err=%v", operation, decoded, err)
		}
		if !ValidOperation(operation) {
			t.Fatalf("pricing vocabulary rejects %s", operation)
		}
	}
}

func TestDecodeAcceptsUnpricedOperations(t *testing.T) {
	for _, operation := range []string{"file", "guardrail"} {
		t.Run(operation, func(t *testing.T) {
			event := metadataEvent()
			event.Operation = operation
			payload, err := Encode(event)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(payload)
			if err != nil || decoded.Operation != operation {
				t.Fatalf("decode %s: event=%v err=%v", operation, decoded, err)
			}
		})
	}
}

func TestDelegatedAttributionRoundTripAndValidation(t *testing.T) {
	base, encodeErr := Encode(metadataEvent())
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	agentID := uuid.NewString()
	memberID := uuid.NewString()
	digest := strings.Repeat("a", 64)
	payload := func(fields string) []byte { return []byte("{" + fields + string(base[1:])) }
	valid := payload(`"agent_principal_id":"` + agentID + `","member_principal_id":"` + memberID + `","delegation_pair_digest":"` + digest + `",`)
	decoded, decodeErr := Decode(valid)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if decoded.AgentPrincipalID != agentID || decoded.MemberPrincipalID != memberID || decoded.DelegationPairDigest != digest {
		t.Fatalf("delegated attribution did not survive decoding: %+v", decoded)
	}
	if _, err := Validate(decoded); err != nil {
		t.Fatalf("valid delegated event rejected: %v", err)
	}
	encoded, reencodeErr := Encode(decoded)
	if reencodeErr != nil {
		t.Fatal(reencodeErr)
	}
	roundTrip, roundTripErr := Decode(encoded)
	if roundTripErr != nil || roundTrip.AgentPrincipalID != agentID || roundTrip.MemberPrincipalID != memberID || roundTrip.DelegationPairDigest != digest {
		t.Fatalf("delegated attribution did not survive round trip: %+v, %v", roundTrip, roundTripErr)
	}
	for name, fields := range map[string]string{
		"missing agent":  `"delegation_pair_digest":"` + digest + `",`,
		"invalid agent":  `"agent_principal_id":"bad","delegation_pair_digest":"` + digest + `",`,
		"missing digest": `"agent_principal_id":"` + agentID + `",`,
		"invalid digest": `"agent_principal_id":"` + agentID + `","delegation_pair_digest":"` + strings.Repeat("A", 64) + `",`,
		"invalid member": `"agent_principal_id":"` + agentID + `","member_principal_id":"bad","delegation_pair_digest":"` + digest + `",`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(payload(fields)); err == nil {
				t.Fatal("invalid delegated attribution accepted")
			}
		})
	}
}

func TestPromptAttributionOnAttempts(t *testing.T) {
	base, encodeErr := Encode(metadataEvent())
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	promptID, revisionID, providerRevisionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	payload := func(prompt string) []byte {
		routing := `"attempts":[{"routing":{"provider_revision_id":"` + providerRevisionID + `","prompt":` + prompt + `},`
		return []byte(strings.Replace(string(base), `"attempts":[{`, routing, 1))
	}
	decoded, decodeErr := Decode(payload(`{"id":"` + promptID + `","revision_id":"` + revisionID + `"}`))
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if _, err := Validate(decoded); err != nil {
		t.Fatal(err)
	}
	prompt := decoded.Attempts[0].Routing.Prompt
	if prompt.ID != promptID || prompt.RevisionID != revisionID {
		t.Fatalf("prompt attribution did not survive decoding: %+v", prompt)
	}
	for name, invalid := range map[string]string{
		"invalid prompt":   `{"id":"bad","revision_id":"` + revisionID + `"}`,
		"invalid revision": `{"id":"` + promptID + `","revision_id":"bad"}`,
		"missing revision": `{"id":"` + promptID + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(payload(invalid)); err == nil {
				t.Fatal("invalid prompt attribution accepted")
			}
		})
	}
}

func TestValidateRejectsUnboundDelegationAttribution(t *testing.T) {
	agent, key := uuid.NewString(), uuid.NewString()
	digest := strings.Repeat("a", 64)
	cases := map[string]*Event{
		"invalid agent":  {RouteSlug: "test", APIKeyID: key, AgentPrincipalID: "bad", DelegationPairDigest: digest},
		"invalid member": {RouteSlug: "test", APIKeyID: key, AgentPrincipalID: agent, MemberPrincipalID: "bad", DelegationPairDigest: digest},
		"missing pair":   {RouteSlug: "test", APIKeyID: key, AgentPrincipalID: agent},
		"missing key":    {RouteSlug: "test", AgentPrincipalID: agent, DelegationPairDigest: digest},
		"keyless origin": {RouteSlug: "test", Origin: OriginShadow, AgentPrincipalID: agent, DelegationPairDigest: digest},
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate(event); err == nil || !strings.Contains(err.Error(), "delegation") {
				t.Fatalf("invalid delegation accepted or misidentified: %v", err)
			}
		})
	}
}

func TestAccountingEventPreservesUsageEvidence(t *testing.T) {
	for _, state := range []struct{ observed, complete, uncertain bool }{
		{false, true, false}, {false, false, true}, {true, false, false}, {true, false, true}, {true, true, false},
	} {
		for _, deferred := range []bool{false, true} {
			event := metadataEvent()
			event.ProviderID, event.UpstreamModel = nil, nil
			event.Committed = false
			event.Attempts[0].Usage = &AttemptUsage{Evidence: EvidenceFrom(state.observed, state.complete, state.uncertain), Deferred: deferred}
			accepted, err := NewAccountingEvent(event)
			if err != nil {
				t.Fatalf("state %+v deferred %v: %v", state, deferred, err)
			}
			recorded := accepted.Validated().Attempts[0].Usage
			if recorded.Evidence.Observed() != state.observed || recorded.Evidence.Complete() != state.complete || recorded.Evidence.Uncertain() != state.uncertain || recorded.Deferred != deferred {
				t.Fatalf("accounting changed evidence %+v deferred %v to %+v", state, deferred, recorded)
			}
			final := event.Attempts[len(event.Attempts)-1]
			if *accepted.Event().ProviderID != final.ProviderID || *accepted.Event().UpstreamModel != final.UpstreamModel || accepted.Event().Committed != final.Committed {
				t.Fatal("accounting request did not derive its final attempt")
			}
		}
	}
}

func TestUsageFingerprintSurvivesReconstruction(t *testing.T) {
	payload := []byte(`{"invocation_count":2,"training_tokens":3,"storage_byte_seconds":"4","cache_token_seconds":"5","container_sessions":{"small":6},"deferred":true,"observed":true,"complete":false,"billing_uncertain":true,"input_tokens":7,"output_tokens":8,"cached_input_tokens":null,"cache_write_input_tokens":null,"cache_write_5m_input_tokens":null,"cache_write_1h_input_tokens":null,"audio_input_tokens":null,"cached_audio_input_tokens":null,"audio_output_tokens":null,"image_input_tokens":null,"cached_image_input_tokens":null,"image_output_tokens":null,"media_units":"9","tool_calls":{"search":10},"batch":true}`)
	var decoded AttemptUsage
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	reconstructed, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(payload) != sha256.Sum256(reconstructed) {
		t.Fatalf("reconstructing persisted usage changed its receipt fingerprint\noriginal %s\nreconstructed %s", payload, reconstructed)
	}
}

func TestRetainedSettlementOriginRoundTripRequiresParent(t *testing.T) {
	event := metadataEvent()
	event.Origin = OriginRetainedSettlement
	event.ParentRequestID = new(event.RequestID)
	encoded, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("retained accounting template cannot settle: %v", err)
	}
	if decoded.Origin != OriginRetainedSettlement || decoded.ParentRequestID == nil || *decoded.ParentRequestID != event.RequestID {
		t.Fatal("lost retained submission parent")
	}
	event.ParentRequestID = nil
	encoded, err = Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Decode(encoded); err == nil {
		t.Fatal("retained child work accepted without its submission parent")
	}
}

func TestPendingGenerationPreservesWholeUsageSnapshots(t *testing.T) {
	initial := AttemptUsage{Evidence: EvidenceObservedIncompleteUncertain,
		UsageQuantities: UsageQuantities{InputTokens: new(int64(100)), OutputTokens: new(int64(20)), CachedInputTokens: new(int64(20))}}
	payload, err := Encode(metadataEvent().WithFinalUsage(initial))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := DecodePendingGeneration(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                              string
		input, output, cached             int64
		state                             GenerationState
		wantInput, wantOutput, wantCached int64
		invalid                           bool
	}{
		{name: "a partial observation cannot combine dimensions", input: 120, output: 10, cached: 30,
			state: GenerationPending, wantInput: 100, wantOutput: 20, wantCached: 20},
		{name: "a terminal provider snapshot replaces provisional evidence", input: 90, output: 10, cached: 10,
			state: GenerationTerminal, wantInput: 90, wantOutput: 10, wantCached: 10},
		{name: "terminal zero is evidence", state: GenerationTerminal},
		{name: "a stale invalid observation is still rejected", input: -1, state: GenerationPending, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := AttemptUsage{Evidence: EvidenceObservedComplete, UsageQuantities: UsageQuantities{
				InputTokens: &tc.input, OutputTokens: &tc.output, CachedInputTokens: &tc.cached, ToolCalls: map[string]int64{"search": 2}, ContainerSessions: map[string]int64{"1g": 1}}}
			observed, err := pending.Observe(evidence, tc.state)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid usage was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = observed.AccountingEvent(time.Now().UTC(), tc.state)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := observed.Encode()
			if err != nil {
				t.Fatal(err)
			}
			*evidence.InputTokens = 999
			evidence.ToolCalls["search"] = 999
			evidence.ContainerSessions["1g"] = 999
			unchanged, err := observed.Encode()
			if err != nil || !bytes.Equal(encoded, unchanged) {
				t.Fatalf("caller changed retained evidence: %v", err)
			}
			event, err := Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			got := event.Attempts[len(event.Attempts)-1].Usage
			if got == nil || got.InputTokens == nil || got.OutputTokens == nil || got.CachedInputTokens == nil ||
				*got.InputTokens != tc.wantInput || *got.OutputTokens != tc.wantOutput || *got.CachedInputTokens != tc.wantCached ||
				event.InputTokens == nil || *event.InputTokens != *got.InputTokens || got.Evidence.Complete() != (tc.state == GenerationTerminal) {
				t.Fatalf("usage snapshot was mixed or lost: %+v", got)
			}
		})
	}
}

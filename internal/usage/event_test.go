package usage

import (
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
	if !validOperation("passthrough") {
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
		if !validOperation(operation) {
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
			entry := samplePriceEntry()
			entry.Operation = operation
			if _, _, err := validatePrices([]Price{entry}, sampleVendorKind); err == nil {
				t.Fatalf("accepted a price for %s", operation)
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
			event.Attempts[0].Usage = &AttemptUsage{Observed: state.observed, Complete: state.complete, BillingUncertain: state.uncertain, Deferred: deferred}
			accepted, err := NewAccountingEvent(event)
			if err != nil {
				t.Fatalf("state %+v deferred %v: %v", state, deferred, err)
			}
			recorded := accepted.validated.Attempts[0].Usage.wire()
			if recorded.Observed != state.observed || recorded.Complete != state.complete || recorded.BillingUncertain != state.uncertain || recorded.Deferred != deferred {
				t.Fatalf("accounting changed evidence %+v deferred %v to %+v", state, deferred, recorded)
			}
			final := event.Attempts[len(event.Attempts)-1]
			if *accepted.event.ProviderID != final.ProviderID || *accepted.event.UpstreamModel != final.UpstreamModel || accepted.event.Committed != final.Committed {
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

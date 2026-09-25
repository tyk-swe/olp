package usage

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func textPointer(value string) *string { return &value }

func TestOutcomeEvidenceRoundTripsBesideRouting(t *testing.T) {
	event := metadataEvent()
	limit := int64(1048576)
	event.Attempts[1].Routing = &Routing{
		ProviderRevisionID: uuid.NewString(),
		Outcome: &OutcomeEvidence{
			NativeStatus:  textPointer("completed"),
			FaultOrigin:   textPointer("provider_transport"),
			FaultScope:    textPointer("endpoint"),
			FaultResource: textPointer("socket"),
			LimitCategory: textPointer("bytes"),
			Limit:         &limit,
		},
	}
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, state, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state != DecodedEvent {
		t.Fatalf("state = %d, want DecodedEvent", state)
	}
	outcome := decoded.Attempts[1].Routing.Outcome
	if outcome == nil || outcome.NativeStatus == nil || *outcome.NativeStatus != "completed" {
		t.Fatalf("outcome lost on round trip: %+v", outcome)
	}
	if outcome.Limit == nil || *outcome.Limit != 1048576 {
		t.Fatalf("configured limit lost: %+v", outcome)
	}
	if _, err := Validate(decoded); err != nil {
		t.Fatalf("validate round trip: %v", err)
	}
}

func TestOutcomeEvidenceAbsentOnRecordsThatPredateIt(t *testing.T) {
	event := metadataEvent()
	event.Attempts[1].Routing = &Routing{ProviderRevisionID: uuid.NewString()}
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var envelope struct {
		Attempts []struct {
			Routing map[string]json.RawMessage `json:"routing"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.Attempts) != 2 {
		t.Fatalf("attempts = %d", len(envelope.Attempts))
	}
	if _, present := envelope.Attempts[1].Routing["outcome"]; present {
		t.Fatal("a record without outcome facts serialized the member")
	}
	decoded, _, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Attempts[1].Routing.Outcome != nil {
		t.Fatal("absent outcome was reconstructed on decode")
	}
}

func TestOutcomeEvidenceRejectsForeignValues(t *testing.T) {
	base := metadataEvent()
	base.Attempts[1].Routing = &Routing{
		ProviderRevisionID: uuid.NewString(),
		Outcome:            &OutcomeEvidence{},
	}
	payload, err := Encode(base)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := []struct {
		name    string
		outcome string
	}{
		{"unknown native status", `{"native_status":"succeeded"}`},
		{"unknown fault origin", `{"fault_origin":"upstream"}`},
		{"unknown fault scope", `{"fault_scope":"wallet"}`},
		{"unknown limit category", `{"limit_category":"rate"}`},
		{"negative configured limit", `{"limit":-1}`},
		{"fractional limit", `{"limit":1.5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := make(map[string]json.RawMessage, len(envelope))
			for key, value := range envelope {
				candidate[key] = value
			}
			var attempts []map[string]json.RawMessage
			if err := json.Unmarshal(candidate["attempts"], &attempts); err != nil {
				t.Fatalf("unmarshal attempts: %v", err)
			}
			var routing map[string]json.RawMessage
			if err := json.Unmarshal(attempts[1]["routing"], &routing); err != nil {
				t.Fatalf("unmarshal routing: %v", err)
			}
			routing["outcome"] = json.RawMessage(tc.outcome)
			attempts[1]["routing"] = marshalForTest(t, routing)
			candidate["attempts"] = marshalForTest(t, attempts)
			if event, _, err := Decode(marshalForTest(t, candidate)); err == nil {
				t.Fatalf("accepted outcome %s: %+v", tc.name, event)
			}
		})
	}
}

func TestValidateRejectsInvalidAttemptOutcome(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OutcomeEvidence)
	}{
		{"unknown native status", func(o *OutcomeEvidence) { o.NativeStatus = textPointer("ok") }},
		{"unknown fault origin", func(o *OutcomeEvidence) { o.FaultOrigin = textPointer("provider") }},
		{"unknown fault scope", func(o *OutcomeEvidence) { o.FaultScope = textPointer("quota") }},
		{"unknown limit category", func(o *OutcomeEvidence) { o.LimitCategory = textPointer("requests") }},
		{"negative limit", func(o *OutcomeEvidence) { negative := int64(-4); o.Limit = &negative }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := metadataEvent()
			outcome := &OutcomeEvidence{}
			tc.mutate(outcome)
			event.Attempts[1].Routing = &Routing{
				ProviderRevisionID: uuid.NewString(),
				Outcome:            outcome,
			}
			validated, err := Validate(event)
			if validated != nil || !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("err = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

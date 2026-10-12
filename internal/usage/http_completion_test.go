package usage

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func pendingHTTPEvent(t *testing.T) *Event {
	t.Helper()
	event := metadataEvent()
	event.HTTPOutcomePending = true
	event.StatusCode, event.ErrorClass, event.FirstByteMS = nil, nil, nil
	event.Committed = false
	final := &event.Attempts[len(event.Attempts)-1]
	final.Committed, final.ErrorClass = false, nil
	final.Routing = &Routing{ProviderRevisionID: event.RuntimeGenerationID, Interaction: &InteractionEvidence{Fidelity: "strict", PlanClass: "native_identity", UpstreamState: UpstreamTerminal, ClientState: ClientUnobserved}}
	if _, err := Validate(event); err != nil {
		t.Fatal(err)
	}
	return event
}

func completionFor(event *Event) HTTPCompletion {
	final := &event.Attempts[len(event.Attempts)-1]
	status, upstreamStatus, failure := 502, 200, "fidelity_protocol_violation"
	return HTTPCompletion{
		EventID: event.EventID, RequestID: event.RequestID, APIKeyID: event.APIKeyID,
		RequestStartedAt: event.RequestStartedAt, CompletedAt: event.RequestStartedAt.Add(100 * time.Millisecond),
		StatusCode: &status, ErrorClass: &failure, LatencyMS: 100, PayloadCaptured: true,
		Attempt: HTTPAttemptCompletion{ID: final.ID, Ordinal: final.Ordinal, ProviderID: final.ProviderID, UpstreamModel: final.UpstreamModel,
			StartedAt: final.StartedAt, CompletedAt: final.StartedAt.Add(50 * time.Millisecond), StatusCode: &upstreamStatus, LatencyMS: 50,
			ClientState: ClientUnobserved},
	}
}

func TestPendingHTTPCodecKeepsOutcomeUnknown(t *testing.T) {
	event := pendingHTTPEvent(t)
	payload, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(event, decoded) {
		t.Fatalf("pending HTTP outcome changed in codec:\n%s", payload)
	}
	decoded.StatusCode = event.Attempts[len(event.Attempts)-1].StatusCode
	if _, err := Validate(decoded); err == nil {
		t.Fatal("pending outcome accepted manufactured request status")
	}
}

func TestPendingHTTPCompletionPreservesFinancialIdentityAndNativeEvidence(t *testing.T) {
	event := pendingHTTPEvent(t)
	financial := *event
	prior := event.Attempts[0]
	usage := *event.Attempts[len(event.Attempts)-1].Usage
	completion := completionFor(event)
	if err := CompletePendingHTTP(event, completion); err != nil {
		t.Fatal(err)
	}
	final := event.Attempts[len(event.Attempts)-1]
	if event.HTTPOutcomePending || event.StatusCode == nil || *event.StatusCode != 502 || event.Committed || final.Committed || event.EventID != financial.EventID || event.RequestID != financial.RequestID || final.ID != completion.Attempt.ID ||
		!reflect.DeepEqual(event.Attempts[0], prior) || !reflect.DeepEqual(*final.Usage, usage) || final.Routing.Interaction.UpstreamState != UpstreamTerminal {
		t.Fatalf("completion rewrote financial or prior attempt evidence: %+v", event)
	}
	payload, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.PayloadCaptured {
		t.Fatal("actual finalized payload capture metadata was lost in the codec")
	}
	if err := CompletePendingHTTP(decoded, completion); err != nil {
		t.Fatalf("exact replay refused: %v", err)
	}
	replayed, _ := json.Marshal(decoded)
	if string(payload) != string(replayed) {
		t.Fatal("exact replay changed the retained template")
	}
	completion.LatencyMS++
	if err := CompletePendingHTTP(decoded, completion); err == nil {
		t.Fatal("conflicting final outcome replaced the first one")
	}
}

func TestPendingHTTPCompletionRejectsDifferentOwner(t *testing.T) {
	for _, mutate := range []func(*HTTPCompletion){
		func(c *HTTPCompletion) { c.EventID = c.RequestID },
		func(c *HTTPCompletion) { c.APIKeyID = c.EventID },
		func(c *HTTPCompletion) { c.Attempt.ID = c.EventID },
		func(c *HTTPCompletion) { c.Attempt.Ordinal-- },
	} {
		event := pendingHTTPEvent(t)
		before, _ := Encode(event)
		completion := completionFor(event)
		mutate(&completion)
		if err := CompletePendingHTTP(event, completion); err == nil {
			t.Fatal("different financial owner accepted")
		}
		after, _ := Encode(event)
		if string(after) != string(before) {
			t.Fatal("rejected owner changed the pending template")
		}
	}
}

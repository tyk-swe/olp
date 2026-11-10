package usage

import (
	"bytes"
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

func TestPendingHTTPCompletionPreservesFinancialIdentityAndNativeEvidence(t *testing.T) {
	event := pendingHTTPEvent(t)
	financial := *event
	prior := event.Attempts[0]
	usage := *event.Attempts[len(event.Attempts)-1].Usage
	completion := completionFor(event)
	payload, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := DecodePendingGeneration(payload)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := pending.CompleteHTTP(completion)
	if err != nil {
		t.Fatal(err)
	}
	completedPayload, err := completed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(completedPayload)
	if err != nil {
		t.Fatal(err)
	}
	final := decoded.Attempts[len(decoded.Attempts)-1]
	if decoded.HTTPOutcomePending || decoded.StatusCode == nil || *decoded.StatusCode != 502 || decoded.Committed || final.Committed || decoded.EventID != financial.EventID || decoded.RequestID != financial.RequestID || final.ID != completion.Attempt.ID ||
		!reflect.DeepEqual(decoded.Attempts[0], prior) || !reflect.DeepEqual(*final.Usage, usage) || final.Routing.Interaction.UpstreamState != UpstreamTerminal {
		t.Fatalf("completion rewrote financial or prior attempt evidence: %+v", decoded)
	}
	if !decoded.PayloadCaptured {
		t.Fatal("actual finalized payload capture metadata was lost in the codec")
	}
	unchanged, err := pending.Encode()
	if err != nil || !bytes.Equal(payload, unchanged) {
		t.Fatalf("completion changed the pending snapshot: %v", err)
	}
	replayed, err := completed.CompleteHTTP(completion)
	if err != nil {
		t.Fatalf("exact replay refused: %v", err)
	}
	replayedPayload, err := replayed.Encode()
	if err != nil || !bytes.Equal(completedPayload, replayedPayload) {
		t.Fatalf("exact replay changed the retained template: %v", err)
	}
	completion.LatencyMS++
	if _, err := completed.CompleteHTTP(completion); err == nil {
		t.Fatal("conflicting final outcome replaced the first one")
	}
}

package usage

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestInteractionEvidenceSurvivesMetadataWireRoundTrip(t *testing.T) {
	event := metadataEvent()
	evidence := &InteractionEvidence{Fidelity: "strict", PlanClass: "native_identity", UpstreamState: UpstreamAccepted, ClientState: ClientUnobserved}
	event.Attempts[0].Routing = &Routing{ProviderRevisionID: uuid.NewString(), Interaction: evidence}
	raw, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil || !reflect.DeepEqual(decoded.Attempts[0].Routing.Interaction, evidence) {
		t.Fatalf("acceptance evidence lost on wire: %v %+v", err, decoded)
	}
	if _, err := Validate(decoded); err != nil {
		t.Fatal(err)
	}
}

func TestInteractionEvidenceRejectsUnknownAndImpossibleStates(t *testing.T) {
	for _, evidence := range []InteractionEvidence{
		{Fidelity: "strict", PlanClass: "native_identity", UpstreamState: "assumed-success", ClientState: ClientUnobserved},
		{Fidelity: "strict", PlanClass: "native_identity", UpstreamState: UpstreamNotSent, ClientState: ClientActionable},
		{Fidelity: "legacy", PlanClass: "native_identity", UpstreamState: UpstreamTerminal, ClientState: ClientTerminal},
	} {
		event := metadataEvent()
		event.Attempts[0].Routing = &Routing{ProviderRevisionID: uuid.NewString(), Interaction: &evidence}
		if _, err := Validate(event); err == nil {
			t.Fatalf("invalid evidence accepted: %+v", evidence)
		}
		raw, err := Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(raw); err == nil {
			t.Fatalf("invalid evidence decoded: %+v", evidence)
		}
	}
}

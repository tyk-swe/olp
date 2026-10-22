package usage

import "testing"

func TestExecutionReferenceRejectsIncompleteIdentity(t *testing.T) {
	id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	valid := ExecutionReference{DispatchID: id, RunID: id, StepID: id, TargetID: id, RouteRevisionID: id}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ExecutionReference{{}, {DispatchID: id, RunID: id}, {DispatchID: id, StepID: id}, {DispatchID: id, TargetID: "native resource"}, {DispatchID: id, RouteRevisionID: "runtime body"}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted incomplete dispatch reference: %#v", bad)
		}
	}
}

func TestExecutionReferenceSurvivesWireRoundTrip(t *testing.T) {
	event := metadataEvent()
	ref := &ExecutionReference{DispatchID: event.EventID, RunID: event.RequestID, StepID: event.EventID}
	event.Attempts[0].Execution = ref
	raw, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Attempts[0].Execution == nil || *decoded.Attempts[0].Execution != *ref {
		t.Fatal("wire decoding dropped execution provenance")
	}
}

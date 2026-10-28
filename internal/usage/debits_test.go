package usage

import (
	"bytes"
	"testing"
)

const debitEventDispatchID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"

func TestDebitDispatchIDRequiresConsistentReferences(t *testing.T) {
	plain := &Event{Attempts: []Attempt{{}}}
	if _, present := debitDispatchID(plain); present {
		t.Fatal("ordinary event routed to debit settlement")
	}
	consistent := &Event{Attempts: []Attempt{
		{Execution: &ExecutionReference{DispatchID: debitEventDispatchID}},
		{Execution: &ExecutionReference{DispatchID: debitEventDispatchID}},
	}}
	if id, present := debitDispatchID(consistent); !present || id != debitEventDispatchID {
		t.Fatalf("dispatch id = %q present=%v", id, present)
	}
	mixed := &Event{Attempts: []Attempt{
		{Execution: &ExecutionReference{DispatchID: debitEventDispatchID}},
		{Execution: &ExecutionReference{DispatchID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"}},
	}}
	if id, present := debitDispatchID(mixed); !present || id != "" {
		t.Fatalf("mixed dispatch references accepted with id %q", id)
	}
}

func TestSessionDebitResultIdentityIsStablePerEvent(t *testing.T) {
	event := &Event{EventID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50609"}
	one := sessionDebitResult(debitEventDispatchID, event, []byte(`{"event":1}`), "0.5", "0.5", true)
	two := sessionDebitResult(debitEventDispatchID, event, []byte(`{"event":1}`), "0.5", "0.5", true)
	if !bytes.Equal(one.ResultDigest, two.ResultDigest) || !bytes.Equal(one.EvidenceDigest, two.EvidenceDigest) {
		t.Fatal("redelivery of one event changed its settlement identity")
	}
	if !one.Final || len(one.ResultDigest) != 32 || len(one.EvidenceDigest) != 32 {
		t.Fatalf("result is not a final digest-bearing settlement: %+v", one)
	}
	other := sessionDebitResult(debitEventDispatchID, &Event{EventID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50610"}, []byte(`{"event":1}`), "0.5", "0.5", true)
	if bytes.Equal(one.ResultDigest, other.ResultDigest) {
		t.Fatal("two events share one settlement identity")
	}
}

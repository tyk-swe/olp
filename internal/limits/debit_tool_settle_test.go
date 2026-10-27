package limits

import (
	"bytes"
	"errors"
	"testing"
)

func toolDebitResult() DebitResult {
	return DebitResult{
		DispatchID: debitTestID, EventID: debitTestID,
		ResultDigest: bytes.Repeat([]byte{1}, 32), EvidenceDigest: bytes.Repeat([]byte{2}, 32),
		Cost: "0", Charge: "0", Final: true, Complete: true,
	}
}

func TestSettleToolDebitRefusesNonZeroOrIncompleteEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*DebitResult){
		"nonzero cost":   func(r *DebitResult) { r.Cost = "0.01" },
		"nonzero charge": func(r *DebitResult) { r.Charge = "1" },
		"not final":      func(r *DebitResult) { r.Final = false },
		"incomplete":     func(r *DebitResult) { r.Complete = false },
		"resolutions":    func(r *DebitResult) { r.ResolvesResultDigests = [][]byte{bytes.Repeat([]byte{3}, 32)} },
	} {
		result := toolDebitResult()
		mutate(&result)
		if err := SettleToolDebitTx(t.Context(), nil, result); !errors.Is(err, ErrDebitConflict) {
			t.Fatalf("%s tool evidence settled instead of conflicting: %v", name, err)
		}
	}
	if !validToolDebitResult(toolDebitResult()) {
		t.Fatal("complete final zero-cost tool evidence refused")
	}
}

func TestDebitResultReplayMatchesOnlyIdenticalEvidence(t *testing.T) {
	result := toolDebitResult()
	resolutionDigest, err := debitResolutionDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := func() (evidence, resolutions []byte, event, cost, charge string, final, complete bool) {
		return result.EvidenceDigest, resolutionDigest, result.EventID, "0.00", "0.000", true, true
	}
	evidence, resolutions, event, cost, charge, final, complete := stored()
	if !debitResultMatches(result, resolutionDigest, evidence, resolutions, event, cost, charge, final, complete) {
		t.Fatal("identical settlement evidence was not recognized as a replay")
	}
	for name, matches := range map[string]bool{
		"evidence":    debitResultMatches(result, resolutionDigest, bytes.Repeat([]byte{9}, 32), resolutions, event, cost, charge, final, complete),
		"resolutions": debitResultMatches(result, resolutionDigest, evidence, bytes.Repeat([]byte{9}, 32), event, cost, charge, final, complete),
		"event":       debitResultMatches(result, resolutionDigest, evidence, resolutions, "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608", cost, charge, final, complete),
		"cost":        debitResultMatches(result, resolutionDigest, evidence, resolutions, event, "0.01", charge, final, complete),
		"charge":      debitResultMatches(result, resolutionDigest, evidence, resolutions, event, cost, "2", final, complete),
		"final":       debitResultMatches(result, resolutionDigest, evidence, resolutions, event, cost, charge, false, complete),
		"complete":    debitResultMatches(result, resolutionDigest, evidence, resolutions, event, cost, charge, final, false),
	} {
		if matches {
			t.Fatalf("changed %s was accepted as a settlement replay", name)
		}
	}
}

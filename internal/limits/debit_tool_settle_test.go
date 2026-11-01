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

func TestSettleToolDebitRefusesIncompleteEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*DebitResult){
		"not final":        func(r *DebitResult) { r.Final = false },
		"incomplete":       func(r *DebitResult) { r.Complete = false },
		"resolutions":      func(r *DebitResult) { r.ResolvesResultDigests = [][]byte{bytes.Repeat([]byte{3}, 32)} },
		"malformed cost":   func(r *DebitResult) { r.Cost = "-1" },
		"malformed charge": func(r *DebitResult) { r.Charge = "1e3" },
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
	priced := toolDebitResult()
	priced.Cost, priced.Charge = "0.25", "0.25"
	if !validToolDebitResult(priced) {
		t.Fatal("complete final priced tool evidence refused")
	}
}

func TestWithinBoundAndAmountEqualShareExactDecimalRules(t *testing.T) {
	if !WithinBound("1.00", "0.40", "0.6") {
		t.Fatal("amount reaching the bound exactly refused")
	}
	if WithinBound("1.00", "0.40", "0.600000000001") {
		t.Fatal("amount one hair past the bound admitted")
	}
	if WithinBound("not-a-number", "0", "0") || WithinBound("1", "", "0") {
		t.Fatal("malformed bound inputs did not fail closed where required")
	}
	if !AmountEqual("0.250", "0.25") || AmountEqual("0.25", "0.26") || AmountEqual("x", "0.25") {
		t.Fatal("decimal equality does not compare values")
	}
}

func TestToolSettleWithinBoundsComparesObservedAgainstBounds(t *testing.T) {
	result := toolDebitResult()
	result.Cost, result.Charge = "0.25", "0.5"
	if !toolSettleWithinBounds("0.25", "0.5", "0", "0", result) {
		t.Fatal("settlement exactly at both bounds refused")
	}
	if !toolSettleWithinBounds("1", "1", "0.75", "0.5", result) {
		t.Fatal("settlement reaching both bounds from prior observed refused")
	}
	for name, bounds := range map[string][4]string{
		"cost over bound":                 {"0.24", "0.5", "0", "0"},
		"charge over bound":               {"0.25", "0.49", "0", "0"},
		"prior cost pushes over":          {"1", "1", "0.76", "0"},
		"prior charge pushes over":        {"1", "1", "0", "0.51"},
		"zero bound with priced evidence": {"0", "0", "0", "0"},
	} {
		if toolSettleWithinBounds(bounds[0], bounds[1], bounds[2], bounds[3], result) {
			t.Fatalf("%s settled beyond its bound", name)
		}
	}
	free := toolDebitResult()
	if !toolSettleWithinBounds("0", "0", "0", "0", free) {
		t.Fatal("explicit zero settlement against zero bounds refused")
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

func TestSettleAgentDebitRejectsIncompleteOrMalformedEvidence(t *testing.T) {
	cases := map[string]DebitResult{
		"not final":               {Cost: "0", Charge: "0", Complete: true},
		"incomplete":              {Cost: "0", Charge: "0", Final: true},
		"negative cost":           {Cost: "-1", Charge: "0", Final: true, Complete: true},
		"invalid charge":          {Cost: "0", Charge: "1e3", Final: true, Complete: true},
		"resolves model evidence": {Cost: "0", Charge: "0", Final: true, Complete: true, ResolvesResultDigests: [][]byte{bytes.Repeat([]byte{3}, 32)}},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			if err := SettleAgentDebitTx(t.Context(), nil, result); !errors.Is(err, ErrDebitConflict) {
				t.Fatalf("agent debit with %s evidence settled: %v", name, err)
			}
		})
	}
}

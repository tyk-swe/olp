package codemode_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestChooseCandidateKeepsTreeThenAvailabilityThenScoreOrder(t *testing.T) {
	candidates := []codemode.Candidate{
		{AccountID: "b", Available: true, Position: 2, Score: 100},
		{AccountID: "a", Available: false, Cooldown: true, Position: 1, Score: 1},
	}
	chosen, ok := codemode.ChooseCandidate(candidates)
	if !ok || chosen.AccountID != "a" {
		t.Fatalf("tree position must dominate availability and score, got %+v ok=%v", chosen, ok)
	}

	scored := []codemode.Candidate{
		{AccountID: "b", Available: true, Position: 7, Score: 2},
		{AccountID: "a", Available: true, Position: 7, Score: 2},
		{AccountID: "c", Available: false, Position: 7, Score: 99},
	}
	chosen, ok = codemode.ChooseCandidate(scored)
	if !ok || chosen.AccountID != "a" {
		t.Fatalf("availability then score then smallest account id must win, got %+v ok=%v", chosen, ok)
	}
	if _, ok = codemode.ChooseCandidate(nil); ok {
		t.Fatal("empty candidate set must not choose")
	}
}

func TestSkippedAccountsNameWhyEachOtherCandidateLost(t *testing.T) {
	candidates := []codemode.Candidate{
		{AccountID: "chosen", Available: true, Position: 1, Score: 5},
		{AccountID: "cooling", Available: false, Cooldown: true, Position: 9, Score: 9},
		{AccountID: "exhausted", Available: false, Position: 9, Score: 9},
		{AccountID: "later-tree", Available: true, Position: 2, Score: 9},
		{AccountID: "outsider", Available: true, Position: 9, Score: 9},
		{AccountID: "same-tier", Available: true, Position: 1, Score: 1},
	}
	chosen, ok := codemode.ChooseCandidate(candidates)
	if !ok || chosen.AccountID != "chosen" {
		t.Fatalf("chosen = %+v ok=%v", chosen, ok)
	}
	skipped := codemode.SkippedAccounts(candidates, chosen)
	want := map[string]codemode.SkipClass{
		"cooling":    codemode.SkipCooldown,
		"exhausted":  codemode.SkipUnavailable,
		"later-tree": codemode.SkipTree,
		"outsider":   codemode.SkipTree,
		"same-tier":  codemode.SkipWeight,
	}
	if len(skipped) != len(want) {
		t.Fatalf("skipped = %+v", skipped)
	}
	previous := ""
	for _, entry := range skipped {
		if want[entry.AccountID] != entry.Class {
			t.Fatalf("skipped %s class = %q, want %q (all %+v)", entry.AccountID, entry.Class, want[entry.AccountID], skipped)
		}
		if entry.AccountID <= previous {
			t.Fatalf("skipped must be ordered by account id: %+v", skipped)
		}
		previous = entry.AccountID
	}
}

func TestSkippedAccountsEmptyForSinglePinnedCandidate(t *testing.T) {
	candidates := []codemode.Candidate{{AccountID: "pinned", Available: true}}
	chosen, ok := codemode.ChooseCandidate(candidates)
	if !ok {
		t.Fatal("single candidate must choose")
	}
	if skipped := codemode.SkippedAccounts(candidates, chosen); len(skipped) != 0 {
		t.Fatalf("pinned evaluation must skip nobody, got %+v", skipped)
	}
}

func TestFreshestEvidenceUsesLatestWindowAndStaysAbsentWithoutAllowance(t *testing.T) {
	early := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	late := early.Add(2 * time.Hour)
	allowance := &codemode.Allowance{
		Windows: []codemode.AllowanceWindow{
			{LimitID: "codex", Window: "primary", Evidence: codemode.AllowanceEvidencePassive, ObservedAt: early},
			{LimitID: "codex", Window: "secondary", Evidence: codemode.AllowanceEvidenceActive, ObservedAt: late},
		},
		ObservedAt: late,
	}
	kind, observed := codemode.FreshestEvidence(allowance)
	if kind == nil || *kind != codemode.AllowanceEvidenceActive || observed == nil || !observed.Equal(late) {
		t.Fatalf("freshest evidence = %v %v", kind, observed)
	}
	if kind, observed := codemode.FreshestEvidence(nil); kind != nil || observed != nil {
		t.Fatalf("missing allowance must stay unknown, got %v %v", kind, observed)
	}
	legacy := &codemode.Allowance{RemainingPercent: new(float64(40)), ObservedAt: early}
	kind, observed = codemode.FreshestEvidence(legacy)
	if kind == nil || *kind != codemode.AllowanceEvidencePassive || observed == nil || !observed.Equal(early) {
		t.Fatalf("legacy top-level evidence = %v %v", kind, observed)
	}
}

func TestDecisionValidateRejectsContentFreeInvariantBreaks(t *testing.T) {
	valid := codemode.Decision{
		AttemptID: "attempt", ChosenAccountID: "chosen", Reason: codemode.DecisionChosen,
		Skipped: []codemode.SkippedAccount{{AccountID: "other", Class: codemode.SkipWeight}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid decision rejected: %v", err)
	}
	cases := map[string]codemode.Decision{
		"unknown reason":    {AttemptID: "attempt", ChosenAccountID: "chosen", Reason: "guessed"},
		"unknown class":     {AttemptID: "attempt", ChosenAccountID: "chosen", Reason: codemode.DecisionChosen, Skipped: []codemode.SkippedAccount{{AccountID: "other", Class: "prompt_body"}}},
		"chosen also skip":  {AttemptID: "attempt", ChosenAccountID: "chosen", Reason: codemode.DecisionPin, Skipped: []codemode.SkippedAccount{{AccountID: "chosen", Class: codemode.SkipTree}}},
		"duplicate skip":    {AttemptID: "attempt", ChosenAccountID: "chosen", Reason: codemode.DecisionChosen, Skipped: []codemode.SkippedAccount{{AccountID: "other", Class: codemode.SkipTree}, {AccountID: "other", Class: codemode.SkipWeight}}},
		"evidence unpinned": {AttemptID: "attempt", ChosenAccountID: "chosen", Reason: codemode.DecisionChosen, Evidence: new(codemode.AllowanceEvidencePassive)},
		"bad ordinal":       {AttemptID: "attempt", ChosenAccountID: "chosen", Reason: codemode.DecisionChosen, RecoveryOrdinal: new(0)},
	}
	for name, decision := range cases {
		if err := decision.Validate(); err == nil {
			t.Fatalf("%s accepted: %+v", name, decision)
		}
	}
}

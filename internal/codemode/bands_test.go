package codemode_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

var bandNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func percentWindow(remaining float64, resetsAt *time.Time, observedAt time.Time) codemode.AllowanceWindow {
	return codemode.AllowanceWindow{LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive, UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetsAt: resetsAt, ObservedAt: observedAt}
}

func bandAllowance(windows ...codemode.AllowanceWindow) *codemode.Allowance {
	return &codemode.Allowance{Windows: windows, ObservedAt: bandNow}
}

func TestAssessBandNeedsAFreshProvenWindow(t *testing.T) {
	future := bandNow.Add(2 * time.Hour)
	futureStale := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow.Add(-3*24*time.Hour))), "codex", bandNow)
	if futureStale.Band != codemode.BandUnknown || futureStale.Evidence != nil {
		t.Fatalf("a future reset must not refresh old observations, got %+v", futureStale)
	}

	stale := codemode.AssessBand(bandAllowance(percentWindow(90, nil, bandNow.Add(-time.Hour))), "codex", bandNow)
	if stale.Band != codemode.BandUnknown || stale.Evidence != nil || stale.ObservedAt != nil {
		t.Fatalf("stale observation must degrade to unknown without evidence, got %+v", stale)
	}

	reset := bandNow.Add(-2 * time.Minute)
	superseded := codemode.AssessBand(bandAllowance(percentWindow(90, &reset, bandNow.Add(-5*time.Minute))), "codex", bandNow)
	if superseded.Band != codemode.BandUnknown {
		t.Fatalf("observation superseded by a past reset must be stale, got %+v", superseded)
	}

	afterReset := codemode.AssessBand(bandAllowance(percentWindow(90, &reset, bandNow.Add(-time.Minute))), "codex", bandNow)
	if afterReset.Band != codemode.BandHealthy {
		t.Fatalf("observation after the reset within fifteen minutes must be fresh, got %+v", afterReset)
	}

	recent := codemode.AssessBand(bandAllowance(percentWindow(90, nil, bandNow.Add(-15*time.Minute))), "codex", bandNow)
	if recent.Band != codemode.BandHealthy {
		t.Fatalf("observation exactly fifteen minutes old must be fresh, got %+v", recent)
	}

	if none := codemode.AssessBand(nil, "codex", bandNow); none.Band != codemode.BandUnknown || none.Evidence != nil {
		t.Fatalf("missing allowance must be unknown, got %+v", none)
	}
	if empty := codemode.AssessBand(bandAllowance(), "codex", bandNow); empty.Band != codemode.BandUnknown {
		t.Fatalf("allowance without windows must be unknown, got %+v", empty)
	}
}

func TestAssessBandLowUsesTightestWindowAtTenPercent(t *testing.T) {
	future := bandNow.Add(time.Hour)
	edge := codemode.AssessBand(bandAllowance(percentWindow(10, &future, bandNow)), "codex", bandNow)
	if edge.Band != codemode.BandLow {
		t.Fatalf("exactly ten percent remaining must be low, got %+v", edge)
	}
	if edge.SoonestReset == nil || !edge.SoonestReset.Equal(future) {
		t.Fatalf("low soonest reset = %v, want %v", edge.SoonestReset, future)
	}
	above := codemode.AssessBand(bandAllowance(percentWindow(10.5, &future, bandNow)), "codex", bandNow)
	if above.Band != codemode.BandHealthy {
		t.Fatalf("above ten percent must stay healthy, got %+v", above)
	}
	secondary := percentWindow(8, &future, bandNow)
	secondary.Window = "secondary"
	resetSecondary := bandNow.Add(30 * time.Minute)
	secondary.ResetsAt = &resetSecondary
	tightest := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), secondary), "codex", bandNow)
	if tightest.Band != codemode.BandLow || tightest.SoonestReset == nil || !tightest.SoonestReset.Equal(resetSecondary) {
		t.Fatalf("tightest window must set band and reset, got %+v", tightest)
	}
}

func TestAssessBandExhaustedFromPercentAndCountWindows(t *testing.T) {
	future := bandNow.Add(time.Hour)
	percent := codemode.AssessBand(bandAllowance(percentWindow(0, &future, bandNow)), "codex", bandNow)
	if percent.Band != codemode.BandExhausted {
		t.Fatalf("zero percent remaining must be exhausted, got %+v", percent)
	}
	count := codemode.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: codemode.AllowanceUnitCount, Evidence: codemode.AllowanceEvidencePassive, RemainingValue: new(float64(0)), ResetsAt: &future, ObservedAt: bandNow}
	if got := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), count), "codex", bandNow); got.Band != codemode.BandExhausted {
		t.Fatalf("zero count remaining must be exhausted, got %+v", got)
	}
	staleCount := count
	staleCount.ResetsAt = nil
	staleCount.ObservedAt = bandNow.Add(-2 * time.Hour)
	if got := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), staleCount), "codex", bandNow); got.Band != codemode.BandHealthy {
		t.Fatalf("stale zero count must not exhaust, got %+v", got)
	}
	lowCount := codemode.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: codemode.AllowanceUnitCount, Evidence: codemode.AllowanceEvidencePassive, UsedValue: new(float64(95)), RemainingValue: new(float64(5)), ResetsAt: &future, ObservedAt: bandNow}
	if got := codemode.AssessBand(bandAllowance(lowCount), "codex", bandNow); got.Band != codemode.BandUnknown || got.Evidence != nil || got.ObservedAt != nil {
		t.Fatalf("count windows never derive a low fraction, got %+v", got)
	}
	spareCount := codemode.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: codemode.AllowanceUnitCount, Evidence: codemode.AllowanceEvidencePassive, RemainingValue: new(float64(5)), ResetsAt: &future, ObservedAt: bandNow}
	if got := codemode.AssessBand(bandAllowance(spareCount), "codex", bandNow); got.Band != codemode.BandUnknown || got.Evidence != nil {
		t.Fatalf("fresh count alone never proves healthy, got %+v", got)
	}
	if got := codemode.AssessBand(bandAllowance(count), "codex", bandNow); got.Band != codemode.BandExhausted {
		t.Fatalf("zero count alone must be exhausted, got %+v", got)
	}
	credits := codemode.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: codemode.AllowanceUnitCredits, Evidence: codemode.AllowanceEvidencePassive, RemainingValue: new(float64(0)), ResetsAt: &future, ObservedAt: bandNow}
	if got := codemode.AssessBand(bandAllowance(credits), "codex", bandNow); got.Band != codemode.BandUnknown {
		t.Fatalf("credits never place an account in a band, got %+v", got)
	}
	cents := codemode.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: codemode.AllowanceUnitCents, Evidence: codemode.AllowanceEvidencePassive, RemainingValue: new(float64(0)), ResetsAt: &future, ObservedAt: bandNow}
	if got := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), cents), "codex", bandNow); got.Band != codemode.BandHealthy {
		t.Fatalf("cents never place an account in a band, got %+v", got)
	}
}

func TestAssessBandEvidenceComesFromDeterminingWindow(t *testing.T) {
	future := bandNow.Add(time.Hour)
	earlier := bandNow.Add(-10 * time.Minute)

	secondary := percentWindow(8, &future, earlier)
	secondary.Window = "secondary"
	secondary.Evidence = codemode.AllowanceEvidencePassive
	low := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), secondary), "codex", bandNow)
	if low.Band != codemode.BandLow || low.Evidence == nil || *low.Evidence != codemode.AllowanceEvidencePassive || low.ObservedAt == nil || !low.ObservedAt.Equal(earlier) {
		t.Fatalf("low evidence must come from the tightest window, got %+v", low)
	}

	tighterHealthy := percentWindow(50, &future, earlier)
	tighterHealthy.Window = "secondary"
	tighterHealthy.Evidence = codemode.AllowanceEvidencePassive
	healthy := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), tighterHealthy), "codex", bandNow)
	if healthy.Band != codemode.BandHealthy || healthy.Evidence == nil || *healthy.Evidence != codemode.AllowanceEvidencePassive || healthy.ObservedAt == nil || !healthy.ObservedAt.Equal(earlier) {
		t.Fatalf("healthy evidence must come from the tightest window, got %+v", healthy)
	}

	freshPercent := percentWindow(75, &future, bandNow)
	freshPercent.Evidence = codemode.AllowanceEvidencePassive
	count := codemode.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: codemode.AllowanceUnitCount, Evidence: codemode.AllowanceEvidencePassive, RemainingValue: new(float64(0)), ResetsAt: &future, ObservedAt: earlier}
	exhausted := codemode.AssessBand(bandAllowance(freshPercent, count), "codex", bandNow)
	if exhausted.Band != codemode.BandExhausted || exhausted.Evidence == nil || *exhausted.Evidence != codemode.AllowanceEvidencePassive || exhausted.ObservedAt == nil || !exhausted.ObservedAt.Equal(earlier) {
		t.Fatalf("exhausted evidence must come from the exhausted window, got %+v", exhausted)
	}
}

func TestAssessBandReadsLegacyTopLevelPercent(t *testing.T) {
	future := bandNow.Add(time.Hour)
	legacy := &codemode.Allowance{RemainingPercent: new(float64(5)), ResetsAt: &future, ObservedAt: bandNow.Add(-time.Minute)}
	got := codemode.AssessBand(legacy, "codex", bandNow)
	if got.Band != codemode.BandLow || got.Evidence == nil || *got.Evidence != codemode.AllowanceEvidencePassive {
		t.Fatalf("legacy top level percent = %+v", got)
	}
}

func TestChooseCandidateOrdersBandsInsideTreeTier(t *testing.T) {
	candidates := []codemode.Candidate{
		{AccountID: "low", Available: true, Position: 7, Score: 100, Band: codemode.BandLow},
		{AccountID: "unknown", Available: true, Position: 7, Score: 50, Band: codemode.BandUnknown},
		{AccountID: "healthy", Available: true, Position: 7, Score: 1, Band: codemode.BandHealthy},
	}
	chosen, ok := codemode.ChooseCandidate(candidates)
	if !ok || chosen.AccountID != "healthy" {
		t.Fatalf("healthy must win over unknown and low regardless of score, got %+v", chosen)
	}
	chosen, ok = codemode.ChooseCandidate(candidates[:2])
	if !ok || chosen.AccountID != "unknown" {
		t.Fatalf("unknown must win over low regardless of score, got %+v", chosen)
	}

	tree := []codemode.Candidate{
		{AccountID: "outsider", Available: true, Position: 2, Score: 100, Band: codemode.BandHealthy},
		{AccountID: "tree", Available: true, Position: 1, Score: 1, Band: codemode.BandLow},
	}
	chosen, ok = codemode.ChooseCandidate(tree)
	if !ok || chosen.AccountID != "tree" {
		t.Fatalf("tree position must dominate band, got %+v", chosen)
	}

	exhausted := []codemode.Candidate{
		{AccountID: "exhausted", Available: true, Position: 3, Score: 100, Band: codemode.BandExhausted},
		{AccountID: "low", Available: true, Position: 3, Score: 1, Band: codemode.BandLow},
	}
	chosen, ok = codemode.ChooseCandidate(exhausted)
	if !ok || chosen.AccountID != "low" || (codemode.Candidate{Available: true, Band: codemode.BandExhausted}).Eligible() {
		t.Fatalf("exhausted must be treated unavailable, got %+v", chosen)
	}

	cooling := []codemode.Candidate{
		{AccountID: "cooling", Available: false, Cooldown: true, Position: 3, Score: 100, Band: codemode.BandHealthy},
		{AccountID: "low", Available: true, Position: 3, Score: 1, Band: codemode.BandLow},
	}
	chosen, ok = codemode.ChooseCandidate(cooling)
	if !ok || chosen.AccountID != "low" {
		t.Fatalf("eligibility must come before band, got %+v", chosen)
	}
}

func TestChooseCandidateKeepsScoreInsideBandAndResetOnlyBreaksLowTies(t *testing.T) {
	soon := bandNow.Add(time.Hour)
	later := bandNow.Add(3 * time.Hour)
	lows := []codemode.Candidate{
		{AccountID: "later-reset", Available: true, Score: 5, Band: codemode.BandLow, ResetAt: &later},
		{AccountID: "soon-reset", Available: true, Score: 5, Band: codemode.BandLow, ResetAt: &soon},
		{AccountID: "no-reset", Available: true, Score: 5, Band: codemode.BandLow},
	}
	chosen, ok := codemode.ChooseCandidate(lows)
	if !ok || chosen.AccountID != "soon-reset" {
		t.Fatalf("soonest reset must break a low band score tie, got %+v", chosen)
	}
	scored := []codemode.Candidate{
		{AccountID: "soon-reset", Available: true, Score: 1, Band: codemode.BandLow, ResetAt: &soon},
		{AccountID: "later-reset", Available: true, Score: 9, Band: codemode.BandLow, ResetAt: &later},
	}
	chosen, ok = codemode.ChooseCandidate(scored)
	if !ok || chosen.AccountID != "later-reset" {
		t.Fatalf("weighted score must pick inside the band before reset, got %+v", chosen)
	}
	healthies := []codemode.Candidate{
		{AccountID: "z-soon", Available: true, Score: 5, Band: codemode.BandHealthy, ResetAt: &soon},
		{AccountID: "a-later", Available: true, Score: 5, Band: codemode.BandHealthy, ResetAt: &later},
	}
	chosen, ok = codemode.ChooseCandidate(healthies)
	if !ok || chosen.AccountID != "a-later" {
		t.Fatalf("reset must not break ties outside the low band, got %+v", chosen)
	}
}

func TestSkippedAccountsNameLowerBandLosses(t *testing.T) {
	candidates := []codemode.Candidate{
		{AccountID: "chosen", Available: true, Position: 4, Score: 1, Band: codemode.BandHealthy},
		{AccountID: "unknown", Available: true, Position: 4, Score: 100, Band: codemode.BandUnknown},
		{AccountID: "low", Available: true, Position: 4, Score: 100, Band: codemode.BandLow},
		{AccountID: "exhausted", Available: true, Position: 4, Score: 100, Band: codemode.BandExhausted},
		{AccountID: "same-band", Available: true, Position: 4, Score: 0, Band: codemode.BandHealthy},
		{AccountID: "later-tree", Available: true, Position: 5, Score: 100, Band: codemode.BandLow},
		{AccountID: "cooling", Available: false, Cooldown: true, Position: 4, Score: 100, Band: codemode.BandLow},
	}
	chosen, ok := codemode.ChooseCandidate(candidates)
	if !ok || chosen.AccountID != "chosen" {
		t.Fatalf("chosen = %+v ok=%v", chosen, ok)
	}
	want := map[string]codemode.SkipClass{
		"unknown":    codemode.SkipBand,
		"low":        codemode.SkipBand,
		"exhausted":  codemode.SkipUnavailable,
		"same-band":  codemode.SkipWeight,
		"later-tree": codemode.SkipTree,
		"cooling":    codemode.SkipCooldown,
	}
	skipped := codemode.SkippedAccounts(candidates, chosen)
	if len(skipped) != len(want) {
		t.Fatalf("skipped = %+v", skipped)
	}
	for _, entry := range skipped {
		if want[entry.AccountID] != entry.Class {
			t.Fatalf("skipped %s class = %q, want %q", entry.AccountID, entry.Class, want[entry.AccountID])
		}
	}
}

func TestAssessBandIgnoresUnqualifiedAndUnrelatedWindows(t *testing.T) {
	future := bandNow.Add(time.Hour)
	unrelated := percentWindow(0, &future, bandNow)
	unrelated.LimitID = "special_model"
	active := percentWindow(0, &future, bandNow)
	active.Window = "secondary"
	active.Evidence = codemode.AllowanceEvidenceActive
	got := codemode.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), unrelated, active), "codex", bandNow)
	if got.Band != codemode.BandHealthy || got.Evidence == nil || *got.Evidence != codemode.AllowanceEvidencePassive {
		t.Fatalf("unrelated or unqualified evidence affected the general Codex band: %+v", got)
	}
}

func TestAssessBandUsesMatchingFamily(t *testing.T) {
	future := bandNow.Add(time.Hour)
	special := percentWindow(0, &future, bandNow)
	special.LimitID = "special_model"
	allowance := bandAllowance(percentWindow(75, &future, bandNow), special)
	if got := codemode.AssessBand(allowance, "special_model", bandNow); got.Band != codemode.BandExhausted {
		t.Fatalf("matching family must use its exhausted window: %+v", got)
	}
}

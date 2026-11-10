package code_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/code"
)

var (
	bandNow           = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	allowanceObserved = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
)

func percentWindow(remaining float64, resetsAt *time.Time, observedAt time.Time) code.AllowanceWindow {
	return code.AllowanceWindow{LimitID: "codex", Window: "primary", Unit: code.AllowanceUnitPercent, Evidence: code.AllowanceEvidencePassive, UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetsAt: resetsAt, ObservedAt: observedAt}
}

func bandAllowance(windows ...code.AllowanceWindow) *code.Allowance {
	return &code.Allowance{Windows: windows, ObservedAt: bandNow}
}

func TestAssessBandNeedsAFreshProvenWindow(t *testing.T) {
	future := bandNow.Add(2 * time.Hour)
	futureStale := code.AssessBand(bandAllowance(percentWindow(75, &future, bandNow.Add(-3*24*time.Hour))), "codex", bandNow)
	if futureStale.Band != code.BandHealthy || futureStale.Evidence == nil {
		t.Fatalf("future reset observation = %+v", futureStale)
	}
	stale := code.AssessBand(bandAllowance(percentWindow(90, nil, bandNow.Add(-time.Hour))), "codex", bandNow)
	if stale.Band != code.BandUnknown || stale.Evidence != nil || stale.ObservedAt != nil {
		t.Fatalf("stale observation = %+v", stale)
	}
	reset := bandNow.Add(-2 * time.Minute)
	if got := code.AssessBand(bandAllowance(percentWindow(90, &reset, bandNow.Add(-5*time.Minute))), "codex", bandNow); got.Band != code.BandUnknown {
		t.Fatalf("superseded observation = %+v", got)
	}
	if got := code.AssessBand(bandAllowance(percentWindow(90, &reset, bandNow.Add(-time.Minute))), "codex", bandNow); got.Band != code.BandHealthy {
		t.Fatalf("post-reset observation = %+v", got)
	}
	if got := code.AssessBand(bandAllowance(percentWindow(90, nil, bandNow.Add(-15*time.Minute))), "codex", bandNow); got.Band != code.BandHealthy {
		t.Fatalf("fifteen minute boundary = %+v", got)
	}
	if none := code.AssessBand(nil, "codex", bandNow); none.Band != code.BandUnknown || none.Evidence != nil {
		t.Fatalf("nil allowance = %+v", none)
	}
	if empty := code.AssessBand(bandAllowance(), "codex", bandNow); empty.Band != code.BandUnknown {
		t.Fatalf("empty allowance = %+v", empty)
	}
}

func TestAssessBandLowAndExhaustedThresholds(t *testing.T) {
	future := bandNow.Add(time.Hour)
	edge := code.AssessBand(bandAllowance(percentWindow(10, &future, bandNow)), "codex", bandNow)
	if edge.Band != code.BandLow || edge.SoonestReset == nil || !edge.SoonestReset.Equal(future) {
		t.Fatalf("ten percent edge = %+v", edge)
	}
	if above := code.AssessBand(bandAllowance(percentWindow(10.5, &future, bandNow)), "codex", bandNow); above.Band != code.BandHealthy {
		t.Fatalf("above ten percent = %+v", above)
	}
	secondary := percentWindow(8, &future, bandNow)
	secondary.Window = "secondary"
	resetSecondary := bandNow.Add(30 * time.Minute)
	secondary.ResetsAt = &resetSecondary
	if tightest := code.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), secondary), "codex", bandNow); tightest.Band != code.BandLow || tightest.SoonestReset == nil || !tightest.SoonestReset.Equal(resetSecondary) {
		t.Fatalf("tightest window = %+v", tightest)
	}
	if percent := code.AssessBand(bandAllowance(percentWindow(0, &future, bandNow)), "codex", bandNow); percent.Band != code.BandExhausted {
		t.Fatalf("zero percent = %+v", percent)
	}
	count := code.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: code.AllowanceUnitCount, Evidence: code.AllowanceEvidencePassive, RemainingValue: new(float64(0)), ResetsAt: &future, ObservedAt: bandNow}
	if got := code.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), count), "codex", bandNow); got.Band != code.BandExhausted {
		t.Fatalf("zero count = %+v", got)
	}
	staleCount := count
	staleCount.ResetsAt = nil
	staleCount.ObservedAt = bandNow.Add(-2 * time.Hour)
	if got := code.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), staleCount), "codex", bandNow); got.Band != code.BandHealthy {
		t.Fatalf("stale zero count = %+v", got)
	}
	spareCount := code.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: code.AllowanceUnitCount, Evidence: code.AllowanceEvidencePassive, RemainingValue: new(float64(5)), ResetsAt: &future, ObservedAt: bandNow}
	if got := code.AssessBand(bandAllowance(spareCount), "codex", bandNow); got.Band != code.BandUnknown || got.Evidence != nil {
		t.Fatalf("spare count = %+v", got)
	}
	credits := code.AllowanceWindow{LimitID: "codex", Window: "monthly", Unit: code.AllowanceUnitCredits, Evidence: code.AllowanceEvidencePassive, RemainingValue: new(float64(0)), ResetsAt: &future, ObservedAt: bandNow}
	if got := code.AssessBand(bandAllowance(credits), "codex", bandNow); got.Band != code.BandUnknown {
		t.Fatalf("credits = %+v", got)
	}
	legacy := &code.Allowance{RemainingPercent: new(float64(5)), ResetsAt: &future, ObservedAt: bandNow.Add(-time.Minute)}
	if got := code.AssessBand(legacy, "codex", bandNow); got.Band != code.BandLow || got.Evidence == nil || *got.Evidence != code.AllowanceEvidencePassive {
		t.Fatalf("legacy percent = %+v", got)
	}
	unrelated := percentWindow(0, &future, bandNow)
	unrelated.LimitID = "special_model"
	active := percentWindow(0, &future, bandNow)
	active.Window = "secondary"
	active.Evidence = code.AllowanceEvidenceActive
	if got := code.AssessBand(bandAllowance(percentWindow(75, &future, bandNow), unrelated, active), "codex", bandNow); got.Band != code.BandHealthy {
		t.Fatalf("unrelated or active window = %+v", got)
	}
}

func TestChooseCandidateAndSkippedAccounts(t *testing.T) {
	soon := bandNow.Add(time.Hour)
	later := bandNow.Add(3 * time.Hour)
	candidates := []code.Candidate{
		{AccountID: "low", Available: true, Position: 7, Score: 100, Band: code.BandLow},
		{AccountID: "unknown", Available: true, Position: 7, Score: 50, Band: code.BandUnknown},
		{AccountID: "healthy", Available: true, Position: 7, Score: 1, Band: code.BandHealthy},
	}
	if chosen, ok := code.ChooseCandidate(candidates); !ok || chosen.AccountID != "healthy" {
		t.Fatalf("healthy must beat unknown and low: %+v", chosen)
	}
	tree := []code.Candidate{
		{AccountID: "outsider", Available: true, Position: 2, Score: 100, Band: code.BandHealthy},
		{AccountID: "tree", Available: true, Position: 1, Score: 1, Band: code.BandLow},
	}
	if chosen, ok := code.ChooseCandidate(tree); !ok || chosen.AccountID != "tree" {
		t.Fatalf("tree position must dominate: %+v", chosen)
	}
	lows := []code.Candidate{
		{AccountID: "later-reset", Available: true, Score: 5, Band: code.BandLow, ResetAt: &later},
		{AccountID: "soon-reset", Available: true, Score: 5, Band: code.BandLow, ResetAt: &soon},
		{AccountID: "no-reset", Available: true, Score: 5, Band: code.BandLow},
	}
	if chosen, ok := code.ChooseCandidate(lows); !ok || chosen.AccountID != "soon-reset" {
		t.Fatalf("soonest reset must break low score tie: %+v", chosen)
	}
	set := []code.Candidate{
		{AccountID: "chosen", Available: true, Position: 4, Score: 1, Band: code.BandHealthy},
		{AccountID: "unknown", Available: true, Position: 4, Score: 100, Band: code.BandUnknown},
		{AccountID: "exhausted", Available: true, Position: 4, Score: 100, Band: code.BandExhausted},
		{AccountID: "same-band", Available: true, Position: 4, Score: 0, Band: code.BandHealthy},
		{AccountID: "later-tree", Available: true, Position: 5, Score: 100, Band: code.BandLow},
		{AccountID: "cooling", Available: false, Cooldown: true, Position: 4, Score: 100, Band: code.BandLow},
	}
	chosen, _ := code.ChooseCandidate(set)
	want := map[string]code.SkipClass{
		"unknown":    code.SkipBand,
		"exhausted":  code.SkipUnavailable,
		"same-band":  code.SkipWeight,
		"later-tree": code.SkipTree,
		"cooling":    code.SkipCooldown,
	}
	skipped := code.SkippedAccounts(set, chosen)
	if len(skipped) != len(want) {
		t.Fatalf("skipped = %+v", skipped)
	}
	for _, entry := range skipped {
		if want[entry.AccountID] != entry.Class {
			t.Fatalf("skipped %s = %q, want %q", entry.AccountID, entry.Class, want[entry.AccountID])
		}
	}
}

func TestAllowanceBoundaryNormalizationMergeAndWithoutActive(t *testing.T) {
	document := []byte(`{
		"windows": [{"limit_id": "codex", "window": "primary", "used_percent": 25, "remaining_percent": 75, "window_minutes": 300, "resets_at": "2026-11-05T15:00:00Z", "observed_at": "2026-11-05T10:00:00Z"}],
		"remaining_tokens": null, "remaining_requests": null, "remaining_percent": 75, "resets_at": "2026-11-05T15:00:00Z", "observed_at": "2026-11-05T10:00:00Z"
	}`)
	var allowance code.Allowance
	if err := json.Unmarshal(document, &allowance); err != nil {
		t.Fatal(err)
	}
	if err := allowance.Validate(); err != nil {
		t.Fatalf("legacy document rejected: %v", err)
	}
	if allowance.Windows[0].Unit != code.AllowanceUnitPercent || allowance.Windows[0].Evidence != code.AllowanceEvidencePassive {
		t.Fatalf("json boundary did not normalize defaults: %+v", allowance.Windows[0])
	}
	later := allowanceObserved.Add(time.Hour)
	incoming := code.Allowance{
		Windows: []code.AllowanceWindow{
			{LimitID: "codex", Window: "primary", Unit: code.AllowanceUnitPercent, Evidence: code.AllowanceEvidenceActive, UsedPercent: 100, RemainingPercent: 0, ObservedAt: later},
			{LimitID: "plan", Window: "monthly", Unit: code.AllowanceUnitCents, Evidence: code.AllowanceEvidenceActive, UsedValue: new(float64(120)), RemainingValue: new(float64(880)), ObservedAt: later},
		},
		Credits:    &code.Credits{HasCredits: true, Balance: new("3.25"), ObservedAt: later},
		ObservedAt: later,
	}
	merged := allowance.Merge(incoming)
	if err := merged.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(merged.Windows) != 3 || merged.RemainingPercent == nil || *merged.RemainingPercent != 75 {
		t.Fatalf("active read overwrote passive mirror: %+v", merged)
	}
	cleared := merged.WithoutActive()
	if len(cleared.Windows) != 1 || cleared.Windows[0].Evidence != code.AllowanceEvidencePassive || cleared.RemainingPercent == nil || *cleared.RemainingPercent != 75 || cleared.Credits == nil {
		t.Fatalf("WithoutActive = %+v", cleared)
	}
	for name, bad := range map[string]code.AllowanceWindow{
		"unknown unit":             {LimitID: "codex", Window: "primary", Unit: "tokens", Evidence: "passive", UsedPercent: 10, RemainingPercent: 90, ObservedAt: allowanceObserved},
		"unknown evidence":         {LimitID: "codex", Window: "primary", Unit: "percent", Evidence: "guessed", UsedPercent: 10, RemainingPercent: 90, ObservedAt: allowanceObserved},
		"unknown window":           {LimitID: "codex", Window: "daily", Unit: "percent", Evidence: "passive", UsedPercent: 10, RemainingPercent: 90, ObservedAt: allowanceObserved},
		"percent with value":       {LimitID: "codex", Window: "primary", Unit: "percent", Evidence: "passive", UsedPercent: 10, RemainingPercent: 90, RemainingValue: new(float64(9)), ObservedAt: allowanceObserved},
		"negative remaining":       {LimitID: "codex", Window: "monthly", Unit: "cents", Evidence: "active", RemainingValue: new(float64(-1)), ObservedAt: allowanceObserved},
		"broken percent pair":      {LimitID: "codex", Window: "primary", Unit: "percent", Evidence: "passive", UsedPercent: 10, RemainingPercent: 50, ObservedAt: allowanceObserved},
		"non-percent with percent": {LimitID: "codex", Window: "monthly", Unit: "count", Evidence: "active", UsedPercent: 10, RemainingPercent: 90, RemainingValue: new(float64(9)), ObservedAt: allowanceObserved},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("%s accepted: %+v", name, bad)
		}
	}
}

func TestCooldownDeadlinesAndCycleReset(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	soon := now.Add(250 * time.Millisecond)
	if got := (&code.Refusal{Status: 503, Code: "code_account_unavailable", RetryAt: &soon}).RetryAfter(now); got != time.Second {
		t.Fatalf("RetryAfter(250ms) = %s", got)
	}
	early := bandNow.Add(10 * time.Minute)
	unavailable := bandNow.Add(time.Hour)
	late := bandNow.Add(2 * time.Hour)
	healthy := percentWindow(80, &early, bandNow)
	healthy.Window = "secondary"
	allowance := bandAllowance(percentWindow(0, &late, bandNow), healthy)
	cooldownRetry := bandNow.Add(30 * time.Minute)
	if deadline := code.BlockingDeadline(bandNow, &unavailable, &cooldownRetry, allowance, false, ""); deadline == nil || !deadline.Equal(late) {
		t.Fatalf("BlockingDeadline = %v, want %v", deadline, late)
	}
	for _, tc := range []struct {
		anchor string
		now    time.Time
		want   time.Time
		ok     bool
	}{
		{"2026-01-15", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), true},
		{"2026-01-15", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), true},
		{"2026-01-31", time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), true},
		{"2028-01-31", time.Date(2028, 1, 31, 10, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC), true},
		{"2026-13-15", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), time.Time{}, false},
	} {
		got, ok := code.NextMonthlyReset(tc.anchor, tc.now)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Fatalf("NextMonthlyReset(%q, %s) = %s, %v want %s, %v", tc.anchor, tc.now, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDecisionConstructorsAndValidation(t *testing.T) {
	scope := code.DecisionScope{ChosenAccountID: "chosen", Skipped: []code.SkippedAccount{{AccountID: "other", Class: code.SkipWeight}}}
	recovery := code.NewRebindRecovery("prior", "codex_usage_limit_reached", 1)
	if _, err := code.NewAttemptDecision(scope, "attempt", code.DecisionChosen, nil, &recovery); err != nil {
		t.Fatalf("valid attempt decision rejected: %v", err)
	}
	if _, err := code.NewConnectionDecision(scope); err != nil {
		t.Fatalf("valid connection decision rejected: %v", err)
	}
	for name, decision := range map[string]code.Decision{
		"unknown reason":       {AttemptID: new("attempt"), ChosenAccountID: "chosen", Reason: "guessed"},
		"chosen also skipped":  {AttemptID: new("attempt"), ChosenAccountID: "chosen", Reason: code.DecisionPin, Skipped: []code.SkippedAccount{{AccountID: "chosen", Class: code.SkipTree}}},
		"evidence unpinned":    {AttemptID: new("attempt"), ChosenAccountID: "chosen", Reason: code.DecisionChosen, Evidence: new(code.AllowanceEvidencePassive)},
		"connection w attempt": {AttemptID: new("attempt"), ChosenAccountID: "chosen", Reason: code.DecisionConnection},
		"chosen no attempt":    {ChosenAccountID: "chosen", Reason: code.DecisionChosen},
		"envelope too long":    {AttemptID: new("attempt"), ChosenAccountID: "chosen", Reason: code.DecisionChosen, RecoveryEnvelope: new(strings.Repeat("a", 101))},
	} {
		if err := decision.Validate(); err == nil {
			t.Fatalf("%s accepted: %+v", name, decision)
		}
	}
}

func TestRouteValidateIncludesModelPools(t *testing.T) {
	route := code.Route{
		Models:         []string{"native-a"},
		ModelRewriting: true,
		ModelPools:     map[string][]string{"native-a": {"native-a"}},
	}
	if err := route.Validate(); err == nil {
		t.Fatal("single-target model pool accepted by Route.Validate")
	}
}

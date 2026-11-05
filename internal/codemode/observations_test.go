package codemode_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

var allowanceObserved = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func floatPtr(value float64) *float64 { return &value }

func TestLegacyAllowanceWindowWithoutUnitOrEvidenceValidatesAndNormalizes(t *testing.T) {
	document := []byte(`{
		"windows": [{"limit_id": "codex", "window": "primary", "used_percent": 25, "remaining_percent": 75, "window_minutes": 300, "resets_at": "2026-11-05T15:00:00Z", "observed_at": "2026-11-05T10:00:00Z"}],
		"remaining_tokens": null, "remaining_requests": null, "remaining_percent": 75, "resets_at": "2026-11-05T15:00:00Z", "observed_at": "2026-11-05T10:00:00Z"
	}`)
	var allowance codemode.Allowance
	if err := json.Unmarshal(document, &allowance); err != nil {
		t.Fatal(err)
	}
	if err := allowance.Validate(); err != nil {
		t.Fatalf("legacy document rejected: %v", err)
	}
	allowance.Normalize()
	window := allowance.Windows[0]
	if window.Unit != "percent" || window.Evidence != "passive" {
		t.Fatalf("legacy defaults not stamped: unit=%q evidence=%q", window.Unit, window.Evidence)
	}
	if window.UsedValue != nil || window.RemainingValue != nil {
		t.Fatalf("percent window gained vendor values: %v %v", window.UsedValue, window.RemainingValue)
	}
	if window.UsedPercent != 25 || window.RemainingPercent != 75 || window.ResetsAt == nil || window.ResetsAt.Hour() != 15 {
		t.Fatalf("legacy values changed: %+v", window)
	}
}

func TestWidenedWindowNamesAndNonPercentValuesValidate(t *testing.T) {
	for _, name := range []string{"primary", "secondary", "five_hour", "weekly", "monthly"} {
		window := codemode.AllowanceWindow{LimitID: "plan", Window: name, Unit: "count", Evidence: "active", UsedValue: floatPtr(40), RemainingValue: floatPtr(60), ObservedAt: allowanceObserved}
		if err := window.Validate(); err != nil {
			t.Fatalf("window %s rejected: %v", name, err)
		}
	}
	unknown := codemode.AllowanceWindow{LimitID: "plan", Window: "monthly", Unit: codemode.AllowanceUnitCents, Evidence: codemode.AllowanceEvidenceActive, ObservedAt: allowanceObserved}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("value-less non-percent window rejected: %v", err)
	}
}

func TestAllowanceWindowRejectsUnknownUnitEvidenceNameAndMixedValues(t *testing.T) {
	base := codemode.AllowanceWindow{LimitID: "codex", Window: "primary", Unit: "percent", Evidence: "passive", UsedPercent: 10, RemainingPercent: 90, ObservedAt: allowanceObserved}
	cases := map[string]codemode.AllowanceWindow{
		"unknown unit":             {LimitID: base.LimitID, Window: base.Window, Unit: "tokens", Evidence: base.Evidence, UsedPercent: 10, RemainingPercent: 90, ObservedAt: base.ObservedAt},
		"unknown evidence":         {LimitID: base.LimitID, Window: base.Window, Unit: base.Unit, Evidence: "guessed", UsedPercent: 10, RemainingPercent: 90, ObservedAt: base.ObservedAt},
		"unknown window":           {LimitID: base.LimitID, Window: "daily", Unit: base.Unit, Evidence: base.Evidence, UsedPercent: 10, RemainingPercent: 90, ObservedAt: base.ObservedAt},
		"percent with value":       {LimitID: base.LimitID, Window: base.Window, Unit: base.Unit, Evidence: base.Evidence, UsedPercent: 10, RemainingPercent: 90, RemainingValue: floatPtr(9), ObservedAt: base.ObservedAt},
		"negative remaining":       {LimitID: base.LimitID, Window: "monthly", Unit: "cents", Evidence: "active", RemainingValue: floatPtr(-1), ObservedAt: base.ObservedAt},
		"broken percent pair":      {LimitID: base.LimitID, Window: base.Window, Unit: base.Unit, Evidence: base.Evidence, UsedPercent: 10, RemainingPercent: 50, ObservedAt: base.ObservedAt},
		"non-percent with percent": {LimitID: base.LimitID, Window: "monthly", Unit: "count", Evidence: "active", UsedPercent: 10, RemainingPercent: 90, RemainingValue: floatPtr(9), ObservedAt: base.ObservedAt},
	}
	for name, window := range cases {
		if err := window.Validate(); err == nil {
			t.Fatalf("%s accepted: %+v", name, window)
		}
	}
}

func TestMergePreservesUnitEvidenceAndValuesAndStampsLegacy(t *testing.T) {
	later := allowanceObserved.Add(time.Hour)
	current := codemode.Allowance{
		Windows:    []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 20, RemainingPercent: 80, ObservedAt: allowanceObserved}},
		ObservedAt: allowanceObserved,
	}
	incoming := codemode.Allowance{
		Windows: []codemode.AllowanceWindow{
			{LimitID: "codex", Window: "primary", Unit: "percent", Evidence: "active", UsedPercent: 30, RemainingPercent: 70, ObservedAt: later},
			{LimitID: "plan", Window: "monthly", Unit: "cents", Evidence: "active", UsedValue: floatPtr(120), RemainingValue: floatPtr(880), ObservedAt: later},
		},
		ObservedAt: later,
	}
	merged := current.Merge(incoming)
	if err := merged.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(merged.Windows) != 2 {
		t.Fatalf("windows = %+v", merged.Windows)
	}
	percent, cents := merged.Windows[0], merged.Windows[1]
	if percent.LimitID != "codex" || percent.Unit != "percent" || percent.Evidence != "active" || percent.RemainingPercent != 70 {
		t.Fatalf("newer percent window not kept: %+v", percent)
	}
	if cents.Unit != "cents" || cents.Evidence != "active" || cents.UsedValue == nil || *cents.UsedValue != 120 || cents.RemainingValue == nil || *cents.RemainingValue != 880 {
		t.Fatalf("vendor values lost in merge: %+v", cents)
	}
	if merged.RemainingPercent == nil || *merged.RemainingPercent != 70 {
		t.Fatalf("top-level percent mirror changed: %v", merged.RemainingPercent)
	}
}

func TestMergeOfLegacyTopLevelPercentSynthesizesStampedWindow(t *testing.T) {
	legacy := codemode.Allowance{RemainingPercent: floatPtr(55), ObservedAt: allowanceObserved}
	merged := legacy.Merge(codemode.Allowance{ObservedAt: allowanceObserved})
	if len(merged.Windows) != 1 {
		t.Fatalf("windows = %+v", merged.Windows)
	}
	window := merged.Windows[0]
	if window.Unit != "percent" || window.Evidence != "passive" || window.RemainingPercent != 55 {
		t.Fatalf("synthesized window = %+v", window)
	}
}

func TestStaleIncomingWindowDoesNotReplaceNewerEvidence(t *testing.T) {
	current := codemode.Allowance{
		Windows:    []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", Unit: "percent", Evidence: "active", UsedPercent: 30, RemainingPercent: 70, ObservedAt: allowanceObserved}},
		ObservedAt: allowanceObserved,
	}
	stale := codemode.Allowance{
		Windows:    []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 90, RemainingPercent: 10, ObservedAt: allowanceObserved.Add(-time.Hour)}},
		ObservedAt: allowanceObserved.Add(-time.Hour),
	}
	merged := current.Merge(stale)
	window := merged.Windows[0]
	if window.RemainingPercent != 70 || window.Evidence != "active" {
		t.Fatalf("stale observation won: %+v", window)
	}
}

package accounting

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestResolveAmount(t *testing.T) {
	str := func(v string) *string { return &v }
	for _, raw := range []*string{nil, str("")} {
		got, err := resolveAmount(raw, "observed_cost")
		if err != nil || got != "0" {
			t.Errorf("resolveAmount(%v) = %q, %v; want 0, nil", raw, got, err)
		}
	}
	got, err := resolveAmount(str("12.340"), "observed_cost")
	if err != nil || got != "12.340" {
		t.Errorf("resolveAmount(decimal) = %q, %v", got, err)
	}
	for _, raw := range []string{"-1", "1e3", "1.", ".5", "1,000", " 1"} {
		if _, err := resolveAmount(str(raw), "observed_cost"); err == nil {
			t.Errorf("resolveAmount accepted %q", raw)
		}
	}
}

func TestValidateResolveInput(t *testing.T) {
	digest := sha256.Sum256([]byte("evidence"))
	valid := hex.EncodeToString(digest[:])
	if err := validateResolveInput(ResolveSettled, valid); err != nil {
		t.Fatalf("validateResolveInput(settled, valid digest): %v", err)
	}
	if err := validateResolveInput(ResolveNotAccepted, valid); err != nil {
		t.Fatalf("validateResolveInput(not_accepted, valid digest): %v", err)
	}
	if err := validateResolveInput(ResolveOutcome("refunded"), valid); err == nil {
		t.Error("validateResolveInput accepted an unknown outcome")
	}
	for _, bad := range []string{"", strings.ToUpper(valid), valid[:32], valid + "00"} {
		if err := validateResolveInput(ResolveSettled, bad); err == nil {
			t.Errorf("validateResolveInput accepted digest %q", bad)
		}
	}
}

func TestCheckResolveBoundsAgainstHeldDebit(t *testing.T) {
	call := ToolCall{CostBound: "10", ChargeBound: "10", ObservedCost: "4", ObservedCharge: "9"}
	if err := checkResolveBounds(call, "6", "1"); err != nil {
		t.Fatalf("checkResolveBounds rejected amounts that reach the bound exactly: %v", err)
	}
	if err := checkResolveBounds(call, "6.01", "0"); err == nil {
		t.Error("checkResolveBounds accepted cost past the held bound")
	}
	if err := checkResolveBounds(call, "0", "1.01"); err == nil {
		t.Error("checkResolveBounds accepted charge past the held bound")
	}
}

func TestResolveOutcomeStatus(t *testing.T) {
	if got := ResolveSettled.status(); got != StatusSettled {
		t.Errorf("ResolveSettled status = %q, want %q", got, StatusSettled)
	}
	if got := ResolveNotAccepted.status(); got != StatusNotAccepted {
		t.Errorf("ResolveNotAccepted status = %q, want %q", got, StatusNotAccepted)
	}
}

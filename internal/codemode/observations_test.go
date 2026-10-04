package codemode

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestOutcomeRejectsUnboundedAndContradictoryMetadata(t *testing.T) {
	now := time.Now().UTC()
	status := func(n int) *int { return &n }
	for _, o := range []Outcome{
		{},
		{Origin: "upstream", Kind: "headers", ObservedAt: now},
		{Origin: "upstream", Kind: "completed", UpstreamStatus: status(500), ObservedAt: now},
		{Origin: "upstream", Kind: "rejected", UpstreamStatus: status(200), ObservedAt: now},
		{Origin: "upstream", Kind: "secret-canary", ObservedAt: now},
		{Origin: "upstream", Kind: "failed", UpstreamStatus: status(600), ObservedAt: now},
		{Origin: "gateway", Kind: "interrupted", UpstreamStatus: status(200), ObservedAt: now},
		{Origin: "client", Kind: "completed", ObservedAt: now},
		{Origin: strings.Repeat("x", 1000), Kind: "failed", ObservedAt: now},
	} {
		if err := o.Validate(); err == nil {
			t.Fatalf("invalid outcome accepted: %+v", o)
		}
	}
	for _, o := range []Outcome{
		{Origin: "upstream", Kind: "headers", UpstreamStatus: status(200), ObservedAt: now},
		{Origin: "upstream", Kind: "completed", ObservedAt: now},
		{Origin: "upstream", Kind: "failed", UpstreamStatus: status(429), ObservedAt: now},
		{Origin: "gateway", Kind: "interrupted", ObservedAt: now},
		{Origin: "gateway", Kind: "transport_error", ObservedAt: now},
		{Origin: "client", Kind: "canceled", ObservedAt: now},
	} {
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAllowanceObservationsBoundIdentityNumbersAndTimestamps(t *testing.T) {
	now := time.Now().UTC()
	w := AllowanceWindow{LimitID: "codex", Window: "primary", UsedPercent: 20, RemainingPercent: 80, ObservedAt: now}
	for _, mutate := range []func(*AllowanceWindow){
		func(w *AllowanceWindow) { w.LimitID = strings.Repeat("x", 101) },
		func(w *AllowanceWindow) { w.Window = "secret-canary" },
		func(w *AllowanceWindow) { w.UsedPercent = math.NaN() },
		func(w *AllowanceWindow) { w.RemainingPercent = math.NaN() },
		func(w *AllowanceWindow) { w.RemainingPercent = 81 },
		func(w *AllowanceWindow) { w.ObservedAt = time.Time{} },
		func(w *AllowanceWindow) { w.ObservedAt = now.Add(time.Second) },
	} {
		invalid := w
		mutate(&invalid)
		if err := (Allowance{ObservedAt: now, Windows: []AllowanceWindow{invalid}}).Validate(); err == nil {
			t.Fatalf("invalid allowance accepted: %+v", invalid)
		}
	}
	if err := (Allowance{ObservedAt: now, Windows: []AllowanceWindow{w, w}}).Validate(); err == nil {
		t.Fatal("duplicate window identities accepted")
	}
	balance := "secret-canary"
	if err := (Credits{Balance: &balance, ObservedAt: now}).Validate(); err == nil {
		t.Fatal("free text balance accepted")
	}
}

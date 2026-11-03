package mcp

import (
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestPricingStateFor(t *testing.T) {
	str := func(v string) *string { return &v }
	cases := []struct {
		price *string
		want  PricingState
	}{
		{nil, PricingUnpriced},
		{str("0"), PricingFree},
		{str("0.00"), PricingFree},
		{str("0.01"), PricingPriced},
		{str("10"), PricingPriced},
	}
	for _, tc := range cases {
		if got := pricingStateFor(tc.price); got != tc.want {
			t.Errorf("pricingStateFor(%v) = %q, want %q", tc.price, got, tc.want)
		}
	}
}

func TestReservationAmountDefaultsToZero(t *testing.T) {
	var nilReservation *Reservation
	if got := nilReservation.amount(); got != "0" {
		t.Errorf("nil reservation amount = %q, want 0", got)
	}
	if got := (&Reservation{}).amount(); got != "0" {
		t.Errorf("unpriced reservation amount = %q, want 0", got)
	}
	price := "2.50"
	if got := (&Reservation{UnitPrice: &price}).amount(); got != price {
		t.Errorf("priced reservation amount = %q, want %q", got, price)
	}
}

func TestResolvedToolsetLookupPrecedence(t *testing.T) {
	resolved := toolsetFromResolution("toolset", "rev", routing.MCPToolsetResolution{
		Tools:   []routing.MCPToolsetTool{{Alias: "live", Name: "live_tool"}, {Alias: "both", Name: "both_tool"}},
		Drifted: []routing.MCPDriftedTool{{Alias: "both"}, {Alias: "drifted"}},
		Invalid: []routing.MCPDriftedTool{{Alias: "invalid"}},
		Missing: []routing.MCPDriftedTool{{Alias: "gone"}},
	})
	cases := []struct {
		alias string
		want  toolLookup
		name  string
	}{
		{"live", toolFound, "live_tool"},
		{"both", toolFound, "both_tool"},
		{"drifted", toolDrifted, ""},
		{"invalid", toolInvalidSchema, ""},
		{"gone", toolServerMissing, ""},
		{"unknown", toolMissing, ""},
	}
	for _, tc := range cases {
		tool, got := resolved.lookup(tc.alias)
		if got != tc.want || tool.Name != tc.name {
			t.Errorf("lookup(%q) = (%q, %v), want (%q, %v)", tc.alias, tool.Name, got, tc.name, tc.want)
		}
	}
}

func TestResolvedToolsetListingFailure(t *testing.T) {
	drifted := resolvedToolset{drifted: []routing.MCPDriftedTool{{Alias: "a"}}, invalid: []routing.MCPDriftedTool{{Alias: "b"}}}
	if got := drifted.listingFailure(); got != errToolDefinitionChanged {
		t.Errorf("drifted listingFailure = %v, want tool definition changed", got)
	}
	invalid := resolvedToolset{invalid: []routing.MCPDriftedTool{{Alias: "b"}}}
	if got := invalid.listingFailure(); got != errToolSchemaInvalid {
		t.Errorf("invalid listingFailure = %v, want tool schema invalid", got)
	}
	missingOnly := resolvedToolset{missing: []routing.MCPDriftedTool{{Alias: "c"}}}
	if got := missingOnly.listingFailure(); got != nil {
		t.Errorf("missing-only listingFailure = %v, want nil", got)
	}
}

func TestCanonicalUUID(t *testing.T) {
	if !canonicalUUID("123e4567-e89b-12d3-a456-426614174000") {
		t.Error("canonicalUUID rejected a canonical UUID")
	}
	for _, raw := range []string{"", "123e4567e89b12d3a456426614174000", "123E4567-E89B-12D3-A456-426614174000", "not-a-uuid"} {
		if canonicalUUID(raw) {
			t.Errorf("canonicalUUID accepted %q", raw)
		}
	}
}

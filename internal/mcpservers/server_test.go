package mcpservers

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

func pricingInput() Input {
	enabled := true
	return Input{
		Name: "Priced tools", Transport: "streamable_http",
		Endpoint: "http://127.0.0.1:8080/mcp", Enabled: &enabled,
	}
}

func invalidField(t *testing.T, err error) string {
	t.Helper()
	var problem *access.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("validation did not report a field problem: %v", err)
	}
	return problem.Field
}

func TestValidateAcceptsPricedFreeAndUnpricedServers(t *testing.T) {
	price, free := "0.25", "0"
	rate, concurrency := int64(120), int64(4)
	for name, mutate := range map[string]func(*Input){
		"unpriced": func(*Input) {},
		"priced": func(in *Input) {
			in.UnitPrice = &price
			in.ToolPrices = map[string]string{"lookup": "0.5"}
			in.CallsPerMinute = &rate
			in.MaxConcurrency = &concurrency
			in.PerKeyCallsPerMinute = &rate
			in.PerKeyMaxConcurrency = &concurrency
		},
		"explicit free": func(in *Input) { in.UnitPrice = &free },
	} {
		in := pricingInput()
		mutate(&in)
		if err := Validate(&in, fixturePolicy()); err != nil {
			t.Fatalf("%s server refused: %v", name, err)
		}
	}
}

func TestValidateRefusesMalformedToolPricingAndLimits(t *testing.T) {
	negative, exponent := "-1", "1e3"
	zero, overRate, overConcurrency := int64(0), routing.MaxToolCallsPerMinute+1, routing.MaxToolConcurrency+1
	for name, tc := range map[string]struct {
		mutate func(*Input)
		field  string
	}{
		"negative price":        {func(in *Input) { in.UnitPrice = &negative }, "unit_price"},
		"exponent price":        {func(in *Input) { in.UnitPrice = &exponent }, "unit_price"},
		"malformed tool price":  {func(in *Input) { in.ToolPrices = map[string]string{"lookup": "-0.5"} }, "tool_prices"},
		"empty tool price name": {func(in *Input) { in.ToolPrices = map[string]string{"": "1"} }, "tool_prices"},
		"zero rate":             {func(in *Input) { in.CallsPerMinute = &zero }, "calls_per_minute"},
		"rate over ceiling":     {func(in *Input) { in.PerKeyCallsPerMinute = &overRate }, "per_key_calls_per_minute"},
		"concurrency over ceiling": {func(in *Input) {
			in.MaxConcurrency = &overConcurrency
		}, "max_concurrency"},
		"per-key concurrency zero": {func(in *Input) { in.PerKeyMaxConcurrency = &zero }, "per_key_max_concurrency"},
	} {
		in := pricingInput()
		tc.mutate(&in)
		err := Validate(&in, fixturePolicy())
		if err == nil || invalidField(t, err) != tc.field {
			t.Fatalf("%s reported %v, want field %s", name, err, tc.field)
		}
	}
}

func TestDefinitionCarriesEffectivePricingAndLimits(t *testing.T) {
	in := pricingInput()
	next := definitionFromInput(in, nil, nil, nil)
	if next.UnitPrice != nil {
		t.Fatalf("unpriced input gained a price: %v", *next.UnitPrice)
	}
	if next.CallsPerMinute != routing.DefaultToolCallsPerMinute || next.MaxConcurrency != routing.DefaultToolMaxConcurrency ||
		next.PerKeyCallsPerMinute != routing.DefaultToolCallsPerMinute || next.PerKeyMaxConcurrency != routing.DefaultToolMaxConcurrency {
		t.Fatalf("defaults did not apply: %+v", next)
	}
	if next.ToolPrices == nil || len(next.ToolPrices) != 0 {
		t.Fatalf("tool prices not normalized to an empty set: %+v", next.ToolPrices)
	}
	price, rate := "0.25", int64(90)
	in.UnitPrice = &price
	in.ToolPrices = map[string]string{"lookup": "0.5"}
	in.CallsPerMinute = &rate
	next = definitionFromInput(in, nil, nil, nil)
	if next.UnitPrice == nil || *next.UnitPrice != price || next.ToolPrices["lookup"] != "0.5" || next.CallsPerMinute != rate {
		t.Fatalf("configured pricing was not carried: %+v", next)
	}
	next.ToolPrices["lookup"] = "9"
	if in.ToolPrices["lookup"] != "0.5" {
		t.Fatal("definition shares the input tool price map")
	}
}

func TestInputPresenceDistinguishesAbsentFromNull(t *testing.T) {
	var absent Input
	if err := absent.UnmarshalJSON([]byte(`{"name":"x","endpoint":"http://127.0.0.1/mcp","enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"unit_price", "tool_prices", "calls_per_minute", "per_key_max_concurrency"} {
		if absent.Present(field) {
			t.Fatalf("absent %s reported present", field)
		}
	}
	var cleared Input
	if err := cleared.UnmarshalJSON([]byte(`{"name":"x","endpoint":"http://127.0.0.1/mcp","enabled":true,"unit_price":null,"calls_per_minute":null}`)); err != nil {
		t.Fatal(err)
	}
	if !cleared.Present("unit_price") || cleared.UnitPrice != nil {
		t.Fatalf("explicit null unit price not recorded as a clearing field: %+v", cleared)
	}
	if !cleared.Present("calls_per_minute") || cleared.CallsPerMinute != nil {
		t.Fatalf("explicit null limit not recorded as present: %+v", cleared)
	}
}

func TestMergeAbsentPricingAndLimitsPreservesAndClears(t *testing.T) {
	price := "0.25"
	previous := &Definition{
		UnitPrice: &price, ToolPrices: map[string]string{"lookup": "0.5"},
		CallsPerMinute: 120, MaxConcurrency: 4, PerKeyCallsPerMinute: 30, PerKeyMaxConcurrency: 2,
	}
	var absent Input
	if err := absent.UnmarshalJSON([]byte(`{"name":"x","endpoint":"http://127.0.0.1/mcp","enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	mergedAbsent := mergeAbsentPricingAndLimits(absent, previous)
	if mergedAbsent.UnitPrice == nil || *mergedAbsent.UnitPrice != price || mergedAbsent.ToolPrices["lookup"] != "0.5" ||
		mergedAbsent.CallsPerMinute == nil || *mergedAbsent.CallsPerMinute != 120 || mergedAbsent.PerKeyMaxConcurrency == nil || *mergedAbsent.PerKeyMaxConcurrency != 2 {
		t.Fatalf("absent fields did not preserve current pricing and limits: %+v", mergedAbsent)
	}
	var cleared Input
	if err := cleared.UnmarshalJSON([]byte(`{"name":"x","endpoint":"http://127.0.0.1/mcp","enabled":true,"unit_price":null,"calls_per_minute":null}`)); err != nil {
		t.Fatal(err)
	}
	mergedCleared := mergeAbsentPricingAndLimits(cleared, previous)
	if mergedCleared.UnitPrice != nil {
		t.Fatalf("null unit price did not clear the price: %+v", mergedCleared)
	}
	if mergedCleared.CallsPerMinute == nil || *mergedCleared.CallsPerMinute != routing.DefaultToolCallsPerMinute {
		t.Fatalf("null limit did not reset to the default: %+v", mergedCleared)
	}
	if mergedCleared.MaxConcurrency == nil || *mergedCleared.MaxConcurrency != 4 {
		t.Fatalf("absent limit beside explicit nulls was not preserved: %+v", mergedCleared)
	}
}

func TestLimitsOnlyUpdateRejectsCatalogAndPriceChanges(t *testing.T) {
	price := "0.25"
	enabled := true
	previous := &Definition{
		Name: "Priced tools", Endpoint: "http://127.0.0.1:8080/mcp", Enabled: true,
		UnitPrice: &price, ToolPrices: map[string]string{"lookup": "0.5"},
		CallsPerMinute: 60, MaxConcurrency: 8, PerKeyCallsPerMinute: 60, PerKeyMaxConcurrency: 8,
	}
	base := func() Input {
		rate := int64(999)
		return Input{
			Name: previous.Name, Endpoint: previous.Endpoint, Enabled: &enabled,
			UnitPrice: &price, ToolPrices: map[string]string{"lookup": "0.5"}, CallsPerMinute: &rate,
		}
	}
	if !limitsOnlyUpdate(base(), previous) {
		t.Fatal("pure limits change was not recognized as limits only")
	}
	same := base()
	sameRate := int64(60)
	same.CallsPerMinute = &sameRate
	if limitsOnlyUpdate(same, previous) {
		t.Fatal("identical update skipped recertification instead of approving the live catalog")
	}
	for name, mutate := range map[string]func(*Input){
		"endpoint":   func(in *Input) { in.Endpoint = "http://127.0.0.1:9999/mcp" },
		"price":      func(in *Input) { changed := "0.30"; in.UnitPrice = &changed },
		"tool price": func(in *Input) { in.ToolPrices = map[string]string{"lookup": "0.6"} },
		"credential": func(in *Input) { in.Credential = []byte("null") },
		"name":       func(in *Input) { in.Name = "Renamed" },
	} {
		in := base()
		mutate(&in)
		if limitsOnlyUpdate(in, previous) {
			t.Fatalf("%s change was treated as limits only", name)
		}
	}
}

func TestValidateRefusesUnapprovedOAuthDestinations(t *testing.T) {
	enabled := true
	for _, endpoint := range []string{"http://metadata.invalid/token", "http://127.0.0.1:8080/token#fragment", "http://user:password@127.0.0.1:8080/token"} {
		in := Input{Name: "Personal tools", Transport: "streamable_http", Endpoint: "http://127.0.0.1:8080/mcp", Enabled: &enabled, OAuth: &routing.MCPOAuth{ClientID: "tool-client", Audience: "tool-api", Scopes: []string{"tools.read"}, AuthorizationEndpoint: "http://127.0.0.1:8080/authorize", TokenEndpoint: endpoint}}
		if err := Validate(&in, fixturePolicy()); err == nil || invalidField(t, err) != "oauth" {
			t.Fatalf("unapproved OAuth destination accepted: %s %v", endpoint, err)
		}
	}
}

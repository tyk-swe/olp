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
	next := definitionFromInput(in, nil)
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
	next = definitionFromInput(in, nil)
	if next.UnitPrice == nil || *next.UnitPrice != price || next.ToolPrices["lookup"] != "0.5" || next.CallsPerMinute != rate {
		t.Fatalf("configured pricing was not carried: %+v", next)
	}
	next.ToolPrices["lookup"] = "9"
	if in.ToolPrices["lookup"] != "0.5" {
		t.Fatal("definition shares the input tool price map")
	}
}

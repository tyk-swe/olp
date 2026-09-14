package usage

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

// usageProblem asserts that an error is a management problem and returns it, so
// a test can pin the status and code the console will actually see.
func usageProblem(t *testing.T, err error) *access.Problem {
	t.Helper()
	var problem *access.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("error %v is not a management problem", err)
	}
	return problem
}

func samplePriceEntry() Price {
	rate := "1.5"
	return Price{ProviderKind: "openai", Model: "gpt-test", Operation: "generation",
		InputPerMillion: &rate, Currency: "USD"}
}

func sampleVendorKind(vendor string) (string, bool) {
	kinds := map[string]string{"openai": "openai", "acme": "openai_compatible"}
	kind, ok := kinds[vendor]
	return kind, ok
}

func TestPriceValidationAcceptsAndNormalizes(t *testing.T) {
	entry := samplePriceEntry()
	entry.Model = "  gpt-test  "
	entry.Currency = " usd "
	vendor := " acme "
	entry.VendorID = &vendor
	entry.ProviderKind = "openai_compatible"
	cached := "0.750000000000"
	entry.CachedInputPerMillion = &cached
	prices, currency, err := validatePrices([]Price{entry}, sampleVendorKind)
	if err != nil {
		t.Fatalf("validate prices: %v", err)
	}
	if currency != "USD" || prices[0].Currency != "USD" {
		t.Fatalf("currency = %q/%q, want USD", currency, prices[0].Currency)
	}
	if prices[0].Model != "gpt-test" || *prices[0].VendorID != "acme" {
		t.Fatalf("normalized entry = %+v", prices[0])
	}
	if *prices[0].CachedInputPerMillion != cached {
		t.Fatalf("cached rate = %q, want the exact stored decimal", *prices[0].CachedInputPerMillion)
	}
}

func TestPriceValidationScopesAreUniqueButIndependent(t *testing.T) {
	first, second := samplePriceEntry(), samplePriceEntry()
	if _, _, err := validatePrices([]Price{first, second}, sampleVendorKind); err == nil {
		t.Fatal("duplicate scoped dimensions were accepted")
	}
	provider := "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
	second.ProviderID = &provider
	if _, _, err := validatePrices([]Price{first, second}, sampleVendorKind); err != nil {
		t.Fatalf("a provider override alongside a default was rejected: %v", err)
	}
	third := samplePriceEntry()
	vendor := "openai"
	third.VendorID = &vendor
	if _, _, err := validatePrices([]Price{first, second, third}, sampleVendorKind); err != nil {
		t.Fatalf("a vendor scope alongside a kind default was rejected: %v", err)
	}
}

func TestPriceValidationRejectsUnusableEntries(t *testing.T) {
	unknownVendor, mismatchedVendor := "nowhere", "acme"
	badProvider := "not-a-uuid"
	zero := "0"
	for _, c := range []struct {
		name   string
		mutate func(*Price)
	}{
		{"an unknown connector kind", func(p *Price) { p.ProviderKind = "quantum" }},
		{"an unknown operation", func(p *Price) { p.Operation = "telepathy" }},
		{"a blank model", func(p *Price) { p.Model = "   " }},
		{"a two letter currency", func(p *Price) { p.Currency = "US" }},
		{"a non alphabetic currency", func(p *Price) { p.Currency = "U$D" }},
		{"no rate at all", func(p *Price) { p.InputPerMillion = nil }},
		{"a cached rate without an input rate", func(p *Price) {
			p.InputPerMillion, p.CachedInputPerMillion = nil, &zero
			p.OutputPerMillion = &zero
		}},
		{"an unknown vendor", func(p *Price) { p.VendorID = &unknownVendor }},
		{"a vendor from another connector", func(p *Price) { p.VendorID = &mismatchedVendor }},
		{"a malformed provider override", func(p *Price) { p.ProviderID = &badProvider }},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry := samplePriceEntry()
			c.mutate(&entry)
			_, _, err := validatePrices([]Price{entry}, sampleVendorKind)
			if err == nil {
				t.Fatal("the entry was accepted")
			}
			if problem := usageProblem(t, err); problem.Status != 422 {
				t.Fatalf("status = %d, want 422", problem.Status)
			}
		})
	}
}

func TestPriceValidationRejectsMixedCurrenciesAndSizes(t *testing.T) {
	first, second := samplePriceEntry(), samplePriceEntry()
	second.Model = "gpt-other"
	second.Currency = "EUR"
	if _, _, err := validatePrices([]Price{first, second}, sampleVendorKind); err == nil {
		t.Fatal("a revision mixing currencies was accepted")
	}
	if _, _, err := validatePrices(nil, sampleVendorKind); err == nil {
		t.Fatal("an empty revision was accepted")
	}
	oversized := make([]Price, MaxRevisionPrices+1)
	for i := range oversized {
		oversized[i] = samplePriceEntry()
	}
	if _, _, err := validatePrices(oversized, sampleVendorKind); err == nil {
		t.Fatal("an oversized revision was accepted")
	}
}

func TestPriceValidationRequiresTheVendorCatalogue(t *testing.T) {
	entry := samplePriceEntry()
	vendor := "openai"
	entry.VendorID = &vendor
	_, _, err := validatePrices([]Price{entry}, nil)
	if err == nil {
		t.Fatal("a vendor scope was accepted without a catalogue")
	}
	var problem *access.Problem
	if errors.As(err, &problem) {
		t.Fatal("an unavailable catalogue was reported as a client error")
	}
}

func TestPriceDecimalsAreExactAndBounded(t *testing.T) {
	for _, valid := range []string{"0", "1", "0.000000000001", "999999999999.999999999999",
		" 12.5 ", "000000000000.5"} {
		normalized, err := normalizeDecimal(valid)
		if err != nil {
			t.Fatalf("decimal %q was rejected: %v", valid, err)
		}
		if normalized != strings.TrimSpace(valid) {
			t.Fatalf("decimal %q was rewritten as %q", valid, normalized)
		}
	}
	for _, invalid := range []string{"", "   ", "-1", "+1", "1.", ".5", "1.2.3", "1e5", "0x1",
		"1234567890123", "0.1234567890123", "1 000", "NaN", "Infinity"} {
		if _, err := normalizeDecimal(invalid); err == nil {
			t.Fatalf("decimal %q was accepted", invalid)
		}
	}
}

package providers

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/usage"
)

// A grant's credential versions come from grant enrollment rather
// than a pasted secret, but it authorizes with one all the same.

// An operation in a native dialect is served through the
// vendor's preset profile.

// A commitment is a positive decimal amount on a UTC calendar period; it is
// part of the stored provider configuration, so it flows through revisions and
// configuration export unchanged.
func TestCommitmentValidation(t *testing.T) {
	cfg := Configuration{Kind: KindOpenAI, AuthMode: "api_key"}
	cfg.Normalize()
	cfg.Options.Commitment = &routing.Commitment{Amount: "300.50", Period: "day"}
	if err := cfg.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, field, amount, period string
	}{
		{"zero", "configuration.options.commitment.amount", "0", "day"},
		{"negative", "configuration.options.commitment.amount", "-1", "day"},
		{"malformed", "configuration.options.commitment.amount", "1.2.3", "day"},
		{"empty amount", "configuration.options.commitment.amount", "", "day"},
		{"long", "configuration.options.commitment.amount", "1234567890123", "day"},
		{"unknown period", "configuration.options.commitment.period", "100", "week"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := cfg
			invalid.Options.Commitment = &routing.Commitment{Amount: tc.amount, Period: tc.period}
			if problem, ok := errors.AsType[*access.Problem](invalid.Validate(&egress.Policy{})); !ok || problem.Field != tc.field {
				t.Fatalf("%+v of %q/%q passed validation", problem, tc.amount, tc.period)
			}
		})
	}
}

func TestCommitmentAndPriceAgreeOnDecimalPrecision(t *testing.T) {
	for _, test := range []struct {
		amount string
		zero   bool
	}{
		{"1", false}, {"0", true}, {"0.000", true}, {"0.000000000001", false},
		{"12.50", false}, {" 1 ", false}, {"1000000000000", false}, {"1.0000000000001", false},
		{"-1", false}, {"1.2.3", false}, {"", false}, {"1e3", false}, {".1", false},
	} {
		t.Run(test.amount, func(t *testing.T) {
			cfg := Configuration{Kind: KindOpenAI, AuthMode: "api_key"}
			cfg.Normalize()
			cfg.Options.Commitment = &routing.Commitment{Amount: test.amount, Period: "day"}
			normalized, priceErr := usage.NormalizePriceAmount(test.amount)
			cfgErr := cfg.Validate(&egress.Policy{})
			if (cfgErr == nil) != (priceErr == nil && !test.zero) {
				t.Fatalf("commitment error = %v, price error = %v", cfgErr, priceErr)
			}
			if cfgErr == nil && cfg.Options.Commitment.Amount != normalized {
				t.Fatalf("commitment amount = %q, price amount = %q", cfg.Options.Commitment.Amount, normalized)
			}
		})
	}
}

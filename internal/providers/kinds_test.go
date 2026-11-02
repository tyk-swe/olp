package providers

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/pricingpolicy"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestProviderAndAccessQuotaBoundariesAgree(t *testing.T) {
	for _, dimension := range []struct {
		name      string
		maximum   int64
		provider  func(*int64) Limits
		admission func(*int64) access.AdmissionLimits
	}{
		{"requests", math.MaxInt32, func(v *int64) Limits { return Limits{RequestsPerMinute: v} }, func(v *int64) access.AdmissionLimits { return access.AdmissionLimits{RequestsPerMinute: v} }},
		{"concurrency", math.MaxInt32, func(v *int64) Limits { return Limits{MaxConcurrency: v} }, func(v *int64) access.AdmissionLimits { return access.AdmissionLimits{MaxConcurrency: v} }},
		{"tokens", limits.MaxCounter, func(v *int64) Limits { return Limits{TokensPerMinute: v} }, func(v *int64) access.AdmissionLimits { return access.AdmissionLimits{TokensPerMinute: v} }},
	} {
		t.Run(dimension.name, func(t *testing.T) {
			if !ValidQuota(dimension.provider(nil)) || dimension.admission(nil).Validate() != nil {
				t.Fatal("absent limit was rejected")
			}
			for _, tc := range []struct {
				value int64
				valid bool
			}{{0, false}, {1, true}, {dimension.maximum, true}, {dimension.maximum + 1, false}} {
				providerValid := ValidQuota(dimension.provider(&tc.value))
				accessErr := dimension.admission(&tc.value).Validate()
				if providerValid != tc.valid || (accessErr == nil) != tc.valid {
					t.Errorf("limit %d, expected %t, provider %t, access %v", tc.value, tc.valid, providerValid, accessErr)
				}
			}
		})
	}
}

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
	small := cfg
	small.Options.Commitment = &routing.Commitment{Amount: "0.000000000001", Period: "day"}
	if err := small.Validate(&egress.Policy{}); err != nil {
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
		{"long fraction", "configuration.options.commitment.amount", "0.0000000000001", "day"},
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

func TestConfigurationNormalizationPreservesSource(t *testing.T) {
	t.Run("empty values", func(t *testing.T) {
		normalized := Configuration{}
		normalized.Normalize()
		if normalized.Options.CredentialHeaders == nil || normalized.Options.PluginOptions != nil {
			t.Fatalf("empty headers and options = %v, %v", normalized.Options.CredentialHeaders, normalized.Options.PluginOptions)
		}
	})
	t.Run("headers", func(t *testing.T) {
		original := Configuration{Options: Options{CredentialHeaders: []string{" x-api-key ", "x-tenant"}}}
		normalized := original
		normalized.Normalize()
		if original.Options.CredentialHeaders[0] != " x-api-key " || normalized.Options.CredentialHeaders[0] != "X-Api-Key" || normalized.Options.CredentialHeaders[1] != "X-Tenant" {
			t.Fatalf("source and normalized headers = %v, %v", original.Options.CredentialHeaders, normalized.Options.CredentialHeaders)
		}
	})
	t.Run("plugin options", func(t *testing.T) {
		original := Configuration{Options: Options{PluginOptions: map[string]string{"unset": "", "kept": "value"}}}
		normalized := original
		normalized.Normalize()
		if _, ok := original.Options.PluginOptions["unset"]; !ok || len(normalized.Options.PluginOptions) != 1 || normalized.Options.PluginOptions["kept"] != "value" {
			t.Fatalf("source and normalized options = %v, %v", original.Options.PluginOptions, normalized.Options.PluginOptions)
		}
	})
	t.Run("commitment", func(t *testing.T) {
		original := Configuration{Kind: KindOpenAI, AuthMode: "api_key"}
		original.Normalize()
		start, end := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
		original.Options.Commitment = &routing.Commitment{ID: "commitment", EffectiveAt: &start, EndsAt: &end, Amount: " 1.25 ", Period: "day"}
		normalized := original
		if err := normalized.Validate(&egress.Policy{}); err != nil {
			t.Fatal(err)
		}
		if original.Options.Commitment.Amount != " 1.25 " || normalized.Options.Commitment.Amount != "1.25" {
			t.Fatalf("source and normalized commitments = %q, %q", original.Options.Commitment.Amount, normalized.Options.Commitment.Amount)
		}
		if normalized.Options.Commitment.ID != original.Options.Commitment.ID || normalized.Options.Commitment.Period != original.Options.Commitment.Period || normalized.Options.Commitment.EffectiveAt == nil || !normalized.Options.Commitment.EffectiveAt.Equal(start) || normalized.Options.Commitment.EndsAt == nil || !normalized.Options.Commitment.EndsAt.Equal(end) {
			t.Fatalf("normalized commitment lost fields: %+v", normalized.Options.Commitment)
		}
	})
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
			normalized, validAmount := pricingpolicy.NormalizeAmount(test.amount)
			cfgErr := cfg.Validate(&egress.Policy{})
			if (cfgErr == nil) != (validAmount && !test.zero) {
				t.Fatalf("commitment error = %v, amount valid = %v", cfgErr, validAmount)
			}
			if cfgErr == nil && cfg.Options.Commitment.Amount != normalized {
				t.Fatalf("commitment amount = %q, price amount = %q", cfg.Options.Commitment.Amount, normalized)
			}
		})
	}
}

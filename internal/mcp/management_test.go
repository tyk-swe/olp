package mcp

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestValidateToolCallFiltersReportsFirstInvalidField(t *testing.T) {
	if err := validateToolCallFilters("", ""); err != nil {
		t.Fatalf("empty filters refused: %v", err)
	}
	if err := validateToolCallFilters(testKeyID, testToolset); err != nil {
		t.Fatalf("canonical filters refused: %v", err)
	}
	for _, tc := range []struct {
		key, toolset, field string
	}{
		{"not-a-uuid", "also-bad", "api_key_id"},
		{testKeyID, "also-bad", "toolset_id"},
		{"0192CF87-D4AB-7F2E-A8B1-C2D3E4F50709", testToolset, "api_key_id"},
		{testKeyID, "00000000-0000-0000-0000-000000000000", "toolset_id"},
	} {
		err := validateToolCallFilters(tc.key, tc.toolset)
		var problem *access.Problem
		if !errors.As(err, &problem) || problem.Field != tc.field || problem.Status != 422 {
			t.Fatalf("filters %q/%q reported %v, want field %s", tc.key, tc.toolset, err, tc.field)
		}
	}
}

func TestValidateResolveInputRequiresKnownOutcomeAndEvidenceDigest(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := validateResolveInput(ResolveSettled, digest); err != nil {
		t.Fatalf("settled outcome refused: %v", err)
	}
	if err := validateResolveInput(ResolveNotAccepted, digest); err != nil {
		t.Fatalf("not_accepted outcome refused: %v", err)
	}
	for _, tc := range []struct {
		outcome       ResolveOutcome
		digest, field string
	}{
		{"", digest, "outcome"},
		{"released", digest, "outcome"},
		{"settled", "", "evidence_digest"},
		{"settled", "xyz", "evidence_digest"},
		{"not_accepted", "0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef", "evidence_digest"},
	} {
		err := validateResolveInput(tc.outcome, tc.digest)
		var problem *access.Problem
		if !errors.As(err, &problem) || problem.Field != tc.field || problem.Status != 422 {
			t.Fatalf("resolve %q/%q reported %v, want field %s", tc.outcome, tc.digest, err, tc.field)
		}
	}
}

func TestResolveAmountDefaultsToZeroAndRejectsMalformed(t *testing.T) {
	if got, err := resolveAmount(nil, "observed_cost"); err != nil || got != "0" {
		t.Fatalf("missing amount did not default to zero: %q %v", got, err)
	}
	amount := "1.25"
	if got, err := resolveAmount(&amount, "observed_cost"); err != nil || got != "1.25" {
		t.Fatalf("valid amount was not carried through: %q %v", got, err)
	}
	for _, bad := range []string{"-1", "1e3", "1.", ".5", " 1"} {
		bad := bad
		if _, err := resolveAmount(&bad, "observed_charge"); err == nil {
			t.Fatalf("malformed amount %q accepted", bad)
		}
	}
}

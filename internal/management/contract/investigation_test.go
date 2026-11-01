package contract_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/management/contract"
)

func TestInvestigationEvidenceKindsMatchDomain(t *testing.T) {
	const project = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	for _, tc := range []struct {
		kind  string
		valid bool
	}{
		{"run", true},
		{"step", true},
		{"approval", true},
		{"span", true},
		{"task", true},
		{"delegation", true},
		{"payload", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			input := agents.InvestigationInput{
				ProjectID: project,
				Filters:   &agents.InvestigationFilters{},
				Evidence:  []agents.InvestigationEvidence{{Kind: tc.kind, ID: project}},
				Summary:   "Review",
			}
			if got := input.Validate() == nil; got != tc.valid {
				t.Fatalf("domain accepts %q = %v, want %v", tc.kind, got, tc.valid)
			}
			if got := contract.InvestigationEvidenceKind(tc.kind).Valid(); got != tc.valid {
				t.Fatalf("write contract accepts %q = %v, want %v", tc.kind, got, tc.valid)
			}
			if got := contract.InvestigationEvidenceViewKind(tc.kind).Valid(); got != tc.valid {
				t.Fatalf("read contract accepts %q = %v, want %v", tc.kind, got, tc.valid)
			}
		})
	}
}

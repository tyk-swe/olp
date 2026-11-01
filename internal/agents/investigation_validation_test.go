package agents_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
)

func TestInvestigationInputValidation(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	stringValue := func(value string) *string { return &value }
	intValue := func(value int64) *int64 { return &value }
	boolValue := func(value bool) *bool { return &value }
	input := func(filters *agents.InvestigationFilters, evidence []agents.InvestigationEvidence) agents.InvestigationInput {
		return agents.InvestigationInput{ProjectID: project, Filters: filters, Evidence: evidence, Summary: "Review"}
	}
	run := agents.InvestigationEvidence{Kind: "run", ID: project}
	allKinds := []agents.InvestigationEvidence{{Kind: "run", ID: project}, {Kind: "step", ID: project}, {Kind: "approval", ID: project}, {Kind: "span", ID: project}, {Kind: "task", ID: project}, {Kind: "delegation", ID: project}}
	for _, tc := range []struct {
		name  string
		input agents.InvestigationInput
		field string
	}{
		{"empty filters", input(&agents.InvestigationFilters{}, []agents.InvestigationEvidence{}), ""},
		{"all filters and evidence kinds", input(&agents.InvestigationFilters{From: stringValue("2026-01-01T00:00:00Z"), To: stringValue("2026-01-02T00:00:00Z"), RunID: &project, MinDurationMS: intValue(0), MaxDurationMS: intValue(100), Error: boolValue(false), Tool: stringValue("lookup")}, allKinds), ""},
		{"maximum evidence", input(&agents.InvestigationFilters{}, slices.Repeat([]agents.InvestigationEvidence{run}, 100)), ""},
		{"too much evidence", input(&agents.InvestigationFilters{}, slices.Repeat([]agents.InvestigationEvidence{run}, 101)), "investigation"},
		{"missing filters", input(nil, []agents.InvestigationEvidence{}), "investigation"},
		{"missing evidence", input(&agents.InvestigationFilters{}, nil), "investigation"},
		{"invalid project", agents.InvestigationInput{ProjectID: "invalid", Filters: &agents.InvestigationFilters{}, Evidence: []agents.InvestigationEvidence{}, Summary: "Review"}, "investigation"},
		{"empty summary", agents.InvestigationInput{ProjectID: project, Filters: &agents.InvestigationFilters{}, Evidence: []agents.InvestigationEvidence{}, Summary: "  "}, "investigation"},
		{"unicode summary at limit", agents.InvestigationInput{ProjectID: project, Filters: &agents.InvestigationFilters{}, Evidence: []agents.InvestigationEvidence{}, Summary: strings.Repeat("é", 4096)}, ""},
		{"oversized summary", agents.InvestigationInput{ProjectID: project, Filters: &agents.InvestigationFilters{}, Evidence: []agents.InvestigationEvidence{}, Summary: strings.Repeat("x", 4097)}, "investigation"},
		{"invalid time", input(&agents.InvestigationFilters{From: stringValue("yesterday")}, []agents.InvestigationEvidence{}), "filters"},
		{"inverted time", input(&agents.InvestigationFilters{From: stringValue("2026-01-01T00:00:00Z"), To: stringValue("0001-01-01T00:00:00Z")}, []agents.InvestigationEvidence{}), "filters"},
		{"invalid run", input(&agents.InvestigationFilters{RunID: stringValue("invalid")}, []agents.InvestigationEvidence{}), "filters"},
		{"negative duration", input(&agents.InvestigationFilters{MinDurationMS: intValue(-1)}, []agents.InvestigationEvidence{}), "filters"},
		{"inverted duration", input(&agents.InvestigationFilters{MinDurationMS: intValue(2), MaxDurationMS: intValue(1)}, []agents.InvestigationEvidence{}), "filters"},
		{"blank tool", input(&agents.InvestigationFilters{Tool: stringValue(" ")}, []agents.InvestigationEvidence{}), "filters"},
		{"unicode tool at limit", input(&agents.InvestigationFilters{Tool: stringValue(strings.Repeat("é", 256))}, []agents.InvestigationEvidence{}), ""},
		{"oversized tool", input(&agents.InvestigationFilters{Tool: stringValue(strings.Repeat("x", 257))}, []agents.InvestigationEvidence{}), "filters"},
		{"unknown evidence", input(&agents.InvestigationFilters{}, []agents.InvestigationEvidence{{Kind: "payload", ID: project}}), "evidence"},
		{"invalid evidence ID", input(&agents.InvestigationFilters{}, []agents.InvestigationEvidence{{Kind: "step", ID: "invalid"}}), "evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.input.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("valid investigation rejected: %v", err)
				}
				return
			}
			var problem *access.Problem
			if !errors.As(err, &problem) || problem.Status != 422 || problem.Field != tc.field {
				t.Fatalf("invalid investigation returned %v, want validation error for %s", err, tc.field)
			}
		})
	}
}

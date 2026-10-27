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

package usage

import "testing"

func TestBudgetBoundaryMetadataIsBoundedAndRequiresARefusal(t *testing.T) {
	for _, tc := range []struct {
		boundary, code string
		valid          bool
	}{
		{"organization", "budget_exhausted", true},
		{"attribution", "budget_exhausted", true},
		{"attribution_budgets.team.private-customer", "budget_exhausted", false},
		{"organization", "rate_limit_exceeded", false},
		{"", "", true},
	} {
		t.Run(tc.boundary+tc.code, func(t *testing.T) {
			event := metadataEvent()
			event.BudgetBoundary = tc.boundary
			if tc.code != "" {
				event.ErrorClass = &tc.code
			}
			if _, err := Validate(event); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			data, err := Encode(event)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(data)
			if (err == nil) != tc.valid {
				t.Fatalf("wire valid=%v: %v", tc.valid, err)
			}
			if tc.valid && decoded.BudgetBoundary != tc.boundary {
				t.Fatal("lost hierarchy evidence")
			}
		})
	}
}

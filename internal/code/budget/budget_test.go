package budget_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code/budget"
)

func TestBudgetWriteRequiresProjectManagementAndPositiveSafeTokenCaps(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	draft, err := budget.ParseDraft(budget.Input{ProjectID: strings.ToUpper(id), DailyTokens: new(int64(100)), Enabled: true})
	if err != nil || draft.ProjectID() != id {
		t.Fatalf("ParseDraft = %+v, %v", draft, err)
	}
	developer := access.Principal{Projects: map[string]string{id: "developer"}}
	var problem *access.Problem
	if _, err := budget.Save(t.Context(), nil, developer, id, id, draft, false); !errors.As(err, &problem) || problem.Status != 403 {
		t.Fatalf("developer saved budget: %v", err)
	}
	if err := (budget.Input{ProjectID: id}).Validate(); err == nil {
		t.Fatal("budget without a token cap accepted")
	}
	for _, tc := range []struct {
		tokens int64
		valid  bool
	}{{-1, false}, {0, false}, {1, true}, {1<<53 - 1, true}, {1 << 53, false}} {
		for _, daily := range []bool{false, true} {
			t.Run(fmt.Sprintf("tokens-%d/daily-%t", tc.tokens, daily), func(t *testing.T) {
				in := budget.Input{ProjectID: id}
				if daily {
					in.DailyTokens = new(tc.tokens)
				} else {
					in.MonthlyTokens = new(tc.tokens)
				}
				if err := in.Validate(); (err == nil) != tc.valid {
					t.Fatalf("valid=%t err=%v", tc.valid, err)
				}
			})
		}
	}
}

package access

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

func TestKeyDebitRequestCarriesKeyBudgetsAndStableLookup(t *testing.T) {
	daily, weekly, monthly := "1", "2", "3"
	authority := Authority{
		ID:              "key-one",
		LookupID:        "lookup-one",
		BudgetIncreases: map[string]string{"key-one": "grant-evidence", "other": "ignored"},
		Policy: KeyPolicy{
			CostBasis:        string(limits.BasisCharge),
			DailyCostLimit:   &daily,
			WeeklyCostLimit:  &weekly,
			MonthlyCostLimit: &monthly,
		},
	}
	request := authority.KeyDebitRequest(time.Hour)
	if request.CostOwnerID != authority.ID || request.LookupID != "lookup-one" || request.CostIncreases != "grant-evidence" || request.LeaseTTL != time.Hour {
		t.Fatalf("key debit request lost its key boundary: %+v", request)
	}
	if request.DailyCostLimit != &daily || request.WeeklyCostLimit != &weekly || request.MonthlyCostLimit != &monthly {
		t.Fatalf("key debit request lost a cost window: %+v", request)
	}
	owner := authority.KeyDebitOwner()
	if owner.ID != request.CostOwnerID || owner.Kind != "key" || owner.Basis != request.CostBasis || request.CostBasis != limits.BasisCharge {
		t.Fatalf("debit owner and request disagree: owner=%+v request=%+v", owner, request)
	}
	authority.LimitLookupID = "rotated-lookup"
	if got := authority.KeyDebitRequest(time.Minute).LookupID; got != "rotated-lookup" {
		t.Fatalf("rotated secret stopped sharing counters: lookup %q", got)
	}
}

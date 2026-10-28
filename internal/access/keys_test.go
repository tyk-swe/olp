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

func TestToolDebitBoundariesCoverEveryCappedBoundary(t *testing.T) {
	limit := "10"
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	group := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602"
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	authority := Authority{
		ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603", LookupID: "lookup-one",
		InstallationID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50604",
		ProjectID:      &project, BudgetGroupID: &group,
		BudgetGroupDailyCostLimit: &limit,
		ProjectBudget:             &BudgetPolicy{DailyCostLimit: &limit},
		EndUserDigest:             digest,
		ProjectEndUserPolicy:      &EndUserPolicy{Defaults: AdmissionLimits{DailyCostLimit: &limit}},
		Policy: KeyPolicy{
			DailyCostLimit: &limit,
			EndUserPolicy:  &EndUserPolicy{Defaults: AdmissionLimits{MonthlyCostLimit: &limit}},
		},
	}
	owners, requests, costBudgeted, ok := authority.ToolDebitBoundaries(time.Hour)
	if !ok || !costBudgeted {
		t.Fatalf("boundaries not resolved as cost budgeted: ok=%v costBudgeted=%v", ok, costBudgeted)
	}
	kinds := map[string]int{}
	for _, owner := range owners {
		kinds[owner.Kind]++
	}
	for _, kind := range []string{"key", "group", "aggregate", "end_user"} {
		if kinds[kind] == 0 {
			t.Fatalf("no %s debit owner recorded: %+v", kind, owners)
		}
	}
	if kinds["end_user"] != 2 {
		t.Fatalf("end-user key and project boundaries not both recorded: %+v", owners)
	}
	if len(requests) != len(owners) {
		t.Fatalf("requests do not cover every owner: owners=%+v requests=%+v", owners, requests)
	}
	for _, owner := range owners {
		found := false
		for _, request := range requests {
			if request.CostOwnerID == owner.ID && request.CostBasis == owner.Basis && request.LeaseTTL == time.Hour {
				found = true
			}
		}
		if !found {
			t.Fatalf("owner has no matching admission request: %+v", owner)
		}
	}
}

func TestToolDebitBoundariesUncappedKeyHasNoCeiling(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	authority := Authority{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603", LookupID: "lookup-one", ProjectID: &project}
	owners, requests, costBudgeted, ok := authority.ToolDebitBoundaries(time.Hour)
	if !ok || costBudgeted {
		t.Fatalf("uncapped authority reported a ceiling: ok=%v costBudgeted=%v", ok, costBudgeted)
	}
	if len(owners) != 1 || len(requests) != 1 || owners[0].Kind != "key" {
		t.Fatalf("uncapped authority gained boundaries: owners=%+v requests=%+v", owners, requests)
	}
	limited := "5"
	authority.OrganizationBudget = &BudgetPolicy{DailyCostLimit: &limited}
	if _, _, _, ok := authority.ToolDebitBoundaries(time.Hour); ok {
		t.Fatal("aggregate boundary without a subject resolved")
	}
}

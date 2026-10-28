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
	keyDaily, groupMonthly := "11", "12"
	installationWeekly, organizationDaily, projectMonthly := "13", "14", "15"
	attributionDaily, endUserProjectMonthly, endUserKeyDaily := "16", "17", "18"
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	group := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602"
	organization := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50605"
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	authority := Authority{
		ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603", LookupID: "lookup-one",
		InstallationID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50604",
		OrganizationID: &organization,
		ProjectID:      &project, BudgetGroupID: &group,
		BudgetGroupCostBasis:        string(limits.BasisCharge),
		BudgetGroupMonthlyCostLimit: &groupMonthly,
		InstallationBudget:          &BudgetPolicy{WeeklyCostLimit: &installationWeekly, CostBasis: "charge"},
		OrganizationBudget:          &BudgetPolicy{DailyCostLimit: &organizationDaily},
		ProjectBudget:               &BudgetPolicy{MonthlyCostLimit: &projectMonthly},
		Attribution:                 map[string]string{"team": "search"},
		ProjectAttributionBudgets:   AttributionBudgets{"team": {"search": BudgetPolicy{DailyCostLimit: &attributionDaily, CostBasis: "charge"}}},
		EndUserDigest:               digest,
		ProjectEndUserPolicy:        &EndUserPolicy{Defaults: AdmissionLimits{MonthlyCostLimit: &endUserProjectMonthly}},
		Policy: KeyPolicy{
			DailyCostLimit: &keyDaily,
			EndUserPolicy:  &EndUserPolicy{Defaults: AdmissionLimits{DailyCostLimit: &endUserKeyDaily}},
		},
	}
	result := authority.ToolDebitBoundaries(time.Hour)
	if !result.Resolved || !result.CostBudgeted {
		t.Fatalf("boundaries not resolved as cost budgeted: %+v", result)
	}
	type want struct {
		id, kind               string
		basis                  limits.Basis
		daily, weekly, monthly *string
		level, subject, value  string
	}
	wants := []want{
		{id: authority.ID, kind: "key", daily: &keyDaily},
		{id: group, kind: "group", basis: limits.BasisCharge, monthly: &groupMonthly},
		{id: limits.AggregateBudgetID("installation", authority.InstallationID), kind: "aggregate", basis: limits.BasisCharge, weekly: &installationWeekly, level: "installation", subject: authority.InstallationID},
		{id: limits.AggregateBudgetID("organization", organization), kind: "aggregate", daily: &organizationDaily, level: "organization", subject: organization},
		{id: limits.AggregateBudgetID("project", project), kind: "aggregate", monthly: &projectMonthly, level: "project", subject: project},
		{id: limits.AttributionBudgetID(project, "team", "search"), kind: "aggregate", basis: limits.BasisCharge, daily: &attributionDaily, level: "attribution", subject: "team", value: "search"},
		{id: limits.EndUserProjectID(project, digest), kind: "end_user", monthly: &endUserProjectMonthly},
		{id: limits.EndUserKeyID(authority.ID, digest), kind: "end_user", daily: &endUserKeyDaily},
	}
	if len(result.Boundaries) != len(wants) {
		t.Fatalf("boundaries = %+v, want %d", result.Boundaries, len(wants))
	}
	for i, expected := range wants {
		got := result.Boundaries[i]
		basis := expected.basis
		if basis == "" {
			basis = limits.BasisCost
		}
		gotBasis := got.Owner.Basis
		if gotBasis == "" {
			gotBasis = limits.BasisCost
		}
		if got.Owner.ID != expected.id || got.Owner.Kind != expected.kind || gotBasis != basis {
			t.Fatalf("boundary %d owner = %+v, want id=%s kind=%s basis=%s", i, got.Owner, expected.id, expected.kind, basis)
		}
		if got.Request.CostOwnerID != expected.id || (got.Request.CostBasis != "" && got.Request.CostBasis != basis) || got.Request.LeaseTTL != time.Hour ||
			got.Request.DailyCostLimit != expected.daily || got.Request.WeeklyCostLimit != expected.weekly || got.Request.MonthlyCostLimit != expected.monthly {
			t.Fatalf("boundary %d request = %+v, want limits daily=%v weekly=%v monthly=%v", i, got.Request, expected.daily, expected.weekly, expected.monthly)
		}
		if got.AggregateLevel != expected.level || got.AggregateKey != expected.subject || got.AggregateValue != expected.value {
			t.Fatalf("boundary %d aggregate identity = %q/%q/%q, want %q/%q/%q", i, got.AggregateLevel, got.AggregateKey, got.AggregateValue, expected.level, expected.subject, expected.value)
		}
	}
	if owners, requests := result.Owners(), result.Requests(); len(owners) != len(wants) || len(requests) != len(wants) {
		t.Fatalf("paired views disagree: owners=%+v requests=%+v", owners, requests)
	}
}

func TestToolDebitBoundariesUncappedKeyHasNoCeiling(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	authority := Authority{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603", LookupID: "lookup-one", ProjectID: &project}
	result := authority.ToolDebitBoundaries(time.Hour)
	if !result.Resolved || result.CostBudgeted {
		t.Fatalf("uncapped authority reported a ceiling: %+v", result)
	}
	if len(result.Boundaries) != 1 || result.Boundaries[0].Owner.Kind != "key" {
		t.Fatalf("uncapped authority gained boundaries: %+v", result.Boundaries)
	}
	limited := "5"
	authority.OrganizationBudget = &BudgetPolicy{DailyCostLimit: &limited}
	if authority.ToolDebitBoundaries(time.Hour).Resolved {
		t.Fatal("aggregate boundary without a subject resolved")
	}
}

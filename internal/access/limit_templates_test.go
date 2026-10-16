package access

import "testing"

func TestLimitTemplatesAreCeilingsWithoutMutatingMembers(t *testing.T) {
	project, name, digest := "project", "standard", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rpm, loose, tpm := int64(2), int64(10), int64(20)
	tiny, large := "0.000000000001", "999999999999.000000000001"
	templates := LimitTemplates{name: {RequestsPerMinute: &rpm, TokensPerMinute: &tpm, DailyCostLimit: &tiny, CostBasis: "charge"}}
	original := &EndUserPolicy{LimitTemplate: &name, Defaults: AdmissionLimits{RequestsPerMinute: &loose}, Overrides: map[string]AdmissionLimits{digest: {}}}
	a := Authority{ProjectID: &project, Policy: KeyPolicy{LimitTemplate: &name, RequestsPerMinute: &loose, DailyCostLimit: &large, EndUserPolicy: original}, ProjectEndUserPolicy: original, BudgetGroupTemplate: &name}
	if err := a.BindLimitTemplates(templates); err != nil {
		t.Fatal(err)
	}
	if *a.Policy.RequestsPerMinute != 2 || *a.Policy.DailyCostLimit != tiny || *a.BudgetGroupTPM != 20 {
		t.Fatal("template ceiling lost")
	}
	if a.Policy.CostBasis != "charge" || a.BudgetGroupCostBasis != "charge" || a.Policy.EndUserPolicy.Defaults.CostBasis != "charge" {
		t.Fatal("template cost basis lost")
	}
	if *a.Policy.EndUserPolicy.Limits(digest).RequestsPerMinute != 2 || *a.ProjectEndUserPolicy.Defaults.RequestsPerMinute != 2 {
		t.Fatal("end-user override removed template")
	}
	if original.Defaults.RequestsPerMinute != &loose || original.Overrides[digest].RequestsPerMinute != nil || loose != 10 {
		t.Fatal("mutated source policy")
	}
	if err := a.BindLimitTemplates(nil); err == nil {
		t.Fatal("missing template allowed")
	}
	a.ProjectID = nil
	if err := a.BindLimitTemplates(templates); err == nil {
		t.Fatal("unassigned template allowed")
	}
}

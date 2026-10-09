package access

import "testing"

func TestLimitTemplatesAreCeilingsWithoutMutatingMembers(t *testing.T) {
	project, name, digest := "project", "standard", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rpm, loose, tpm := int64(2), int64(10), int64(20)
	tiny, large := "0.000000000001", "999999999999.000000000001"
	templates := LimitTemplates{name: {RequestsPerMinute: &rpm, TokensPerMinute: &tpm, DailyCostLimit: &tiny}}
	original := &EndUserPolicy{LimitTemplate: &name, Defaults: AdmissionLimits{RequestsPerMinute: &loose}, Overrides: map[string]AdmissionLimits{digest: {}}}
	a := Authority{ProjectID: &project, Policy: KeyPolicy{LimitTemplate: &name, RequestsPerMinute: &loose, DailyCostLimit: &large, EndUserPolicy: original}, ProjectEndUserPolicy: original, BudgetGroupTemplate: &name}
	if err := a.BindLimitTemplates(templates); err != nil {
		t.Fatal(err)
	}
	if *a.Policy.RequestsPerMinute != 2 || *a.Policy.DailyCostLimit != tiny || *a.BudgetGroupTPM != 20 {
		t.Fatal("template ceiling lost")
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
func TestUnconfiguredLimitTemplatesAddNoAllocations(t *testing.T) {
	a := Authority{}
	if n := testing.AllocsPerRun(100, func() {
		if err := a.BindLimitTemplates(nil); err != nil {
			panic(err)
		}
	}); n != 0 {
		t.Fatalf("allocations=%v", n)
	}
}

func TestTemplateValidationCanonicalizesCostsWithoutChangingOtherPolicyPointers(t *testing.T) {
	amount := " 0.000000000001 "
	templates := LimitTemplates{"precise": {DailyCostLimit: &amount}}
	if err := templates.Validate(); err != nil {
		t.Fatal(err)
	}
	if *templates["precise"].DailyCostLimit != "0.000000000001" || amount != " 0.000000000001 " {
		t.Fatal("template validation lost canonical cost or changed another owner")
	}
}

func TestWeeklyTemplateCeilingsPreserveSourcePointers(t *testing.T) {
	name, project, base, local := "weekly", "project", " 4.000000000001 ", "8"
	templates := LimitTemplates{name: {WeeklyCostLimit: &base}}
	a := Authority{ProjectID: &project, Policy: KeyPolicy{LimitTemplate: &name, WeeklyCostLimit: &local, EndUserPolicy: &EndUserPolicy{LimitTemplate: &name}}, BudgetGroupTemplate: &name}
	if err := a.BindLimitTemplates(templates); err != nil {
		t.Fatal(err)
	}
	for _, amount := range []*string{a.Policy.WeeklyCostLimit, a.Policy.EndUserPolicy.Defaults.WeeklyCostLimit, a.BudgetGroupWeeklyCostLimit} {
		if amount == nil || *amount != "4.000000000001" {
			t.Fatalf("weekly ceiling missing: %v", amount)
		}
	}
	if base != " 4.000000000001 " || local != "8" {
		t.Fatal("compilation changed source policy")
	}
}

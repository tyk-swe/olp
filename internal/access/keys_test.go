package access

import (
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestBudgetAndTemplateValidationNormalizeCostLimits(t *testing.T) {
	budgetAmount, templateAmount := " 4.50 ", " 5.75 "
	budgets := keyauthority.AttributionBudgets{"team": {"app": {DailyCostLimit: &budgetAmount}}}
	templates := keyauthority.LimitTemplates{"template": {MonthlyCostLimit: &templateAmount}}
	if err := budgets.ValidateAndNormalize(); err != nil {
		t.Fatal(err)
	}
	if err := templates.ValidateAndNormalize(); err != nil {
		t.Fatal(err)
	}
	if got := *budgets["team"]["app"].DailyCostLimit; got != "4.50" {
		t.Errorf("attribution budget = %q", got)
	}
	if got := *templates["template"].MonthlyCostLimit; got != "5.75" {
		t.Errorf("template limit = %q", got)
	}
	if templateAmount != " 5.75 " {
		t.Errorf("template source amount = %q", templateAmount)
	}
}

func TestTaskAuthorityKeepsOwnerIdentityAndBindingLimits(t *testing.T) {
	project, issuer := "project", "issuer"
	daily := "7"
	expires := time.Now().Add(time.Hour)
	policy := keyauthority.Authority{
		Issuer: "binding", ProjectID: &project,
		Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"allowed"}, DailyCostLimit: &daily},
	}
	owner := keyauthority.Authority{ID: "owner", LookupID: "owner-lookup", LimitLookupID: "owner-limits", WorkloadIssuerID: &issuer, WorkloadDigest: "workload", WorkloadMapping: "mapping"}
	capability := &keyauthority.TaskCapability{Digest: "capability", RouteRevisions: []string{"revision"}}
	callback := policy.TaskAuthority(keyauthority.TaskDerivation{
		Identity:         owner,
		SnapshotID:       "snapshot",
		WorkloadRevision: "workload-revision",
		ExpiresAt:        &expires,
		EndUserDigest:    "end-user",
		Capability:       capability,
	})
	if !callback.Allows("inference", "allowed", &project, time.Now()) || callback.Allows("inference", "other", &project, time.Now()) {
		t.Fatal("task authority did not retain the binding's route policy")
	}
	debit := callback.KeyCostBoundary()
	if debit.Owner.ID != owner.ID || debit.Lookup != owner.LimitLookupID || debit.Budget == nil || debit.Budget.Daily == nil || *debit.Budget.Daily != daily {
		t.Fatalf("task debit did not retain owner and budget boundaries: %+v", debit)
	}
	if callback.SnapshotID != "snapshot" || callback.WorkloadIssuerID == nil || *callback.WorkloadIssuerID != issuer || callback.WorkloadRevision != "workload-revision" || callback.WorkloadDigest != owner.WorkloadDigest || callback.WorkloadMapping != owner.WorkloadMapping || callback.EndUserDigest != "end-user" || callback.TaskCapability != capability || callback.ExpiresAt == nil || !callback.ExpiresAt.Equal(expires) {
		t.Fatalf("task identity or lifetime was not retained: %+v", callback)
	}
	if policy.TaskCapability != nil || policy.EndUserDigest != "" || policy.ID != "" {
		t.Fatal("constructing a task authority changed the source binding")
	}
}

func TestDerivedAuthoritiesFollowTheSourceKeyPause(t *testing.T) {
	project := "project"
	now := time.Now()
	expires := now.Add(time.Hour)
	for _, pausedAt := range []*time.Time{nil, &now} {
		key := keyauthority.Authority{ID: "key", ProjectID: &project, PausedAt: pausedAt, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}}
		derived := map[string]keyauthority.Authority{
			"task": key.TaskAuthority(keyauthority.TaskDerivation{
				Identity:   key,
				SnapshotID: "snapshot",
				ExpiresAt:  &expires,
			}),
			"delegation": *key.Delegated(keyauthority.DelegationDerivation{
				Scopes:      []string{"inference"},
				Agent:       keyauthority.Authority{ID: "agent"},
				MemberID:    "member",
				PairDigest:  "pair",
				TokenDigest: "token",
				ExpiresAt:   expires,
			}),
		}
		for name, authority := range derived {
			if got, want := authority.Allows("inference", "route", &project, now), pausedAt == nil; got != want {
				t.Fatalf("%s authority from key paused=%v allows = %v, want %v", name, pausedAt != nil, got, want)
			}
		}
	}
}

func TestKeyCostBoundaryCarriesKeyBudgetsAndStableLookup(t *testing.T) {
	daily, weekly, monthly := "1", "2", "3"
	authority := keyauthority.Authority{
		ID:              "key-one",
		LookupID:        "lookup-one",
		BudgetIncreases: map[string]string{"key-one": "grant-evidence", "other": "ignored"},
		Policy: keyauthority.KeyPolicy{
			CostBasis:        string(limits.BasisCharge),
			DailyCostLimit:   &daily,
			WeeklyCostLimit:  &weekly,
			MonthlyCostLimit: &monthly,
		},
	}
	boundary := authority.KeyCostBoundary()
	if boundary.Owner != (limits.Owner{ID: authority.ID, Kind: limits.OwnerKey, Basis: limits.BasisCharge}) || boundary.Budget.Owner != boundary.Owner {
		t.Fatalf("key boundary lost its owner: %+v", boundary)
	}
	if boundary.Lookup != "lookup-one" || boundary.Budget.Increases != "grant-evidence" {
		t.Fatalf("key boundary lost its lookup or increases: %+v", boundary)
	}
	if boundary.Budget.Daily != &daily || boundary.Budget.Weekly != &weekly || boundary.Budget.Monthly != &monthly {
		t.Fatalf("key boundary lost a cost window: %+v", boundary.Budget)
	}
	authority.LimitLookupID = "rotated-lookup"
	if got := authority.KeyCostBoundary().Lookup; got != "rotated-lookup" {
		t.Fatalf("rotated secret stopped sharing counters: lookup %q", got)
	}
}

func TestCostBoundariesCoverEveryCappedBoundary(t *testing.T) {
	keyDaily, groupMonthly := "11", "12"
	installationWeekly, organizationDaily, projectMonthly := "13", "14", "15"
	attributionDaily, endUserProjectMonthly, endUserKeyDaily, routeDaily := "16", "17", "18", "19"
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	group := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602"
	organization := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50605"
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	authority := keyauthority.Authority{
		ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603", LookupID: "lookup-one",
		InstallationID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50604",
		OrganizationID: &organization,
		ProjectID:      &project, BudgetGroupID: &group,
		BudgetGroupCostBasis:        string(limits.BasisCharge),
		BudgetGroupMonthlyCostLimit: &groupMonthly,
		InstallationBudget:          &keyauthority.BudgetPolicy{WeeklyCostLimit: &installationWeekly, CostBasis: "charge"},
		OrganizationBudget:          &keyauthority.BudgetPolicy{DailyCostLimit: &organizationDaily},
		ProjectBudget:               &keyauthority.BudgetPolicy{MonthlyCostLimit: &projectMonthly},
		Attribution:                 map[string]string{"team": "search"},
		ProjectAttributionBudgets:   keyauthority.AttributionBudgets{"team": {"search": keyauthority.BudgetPolicy{DailyCostLimit: &attributionDaily, CostBasis: "charge"}}},
		EndUserDigest:               digest,
		ProjectEndUserPolicy:        &keyauthority.EndUserPolicy{Defaults: keyauthority.AdmissionLimits{MonthlyCostLimit: &endUserProjectMonthly}},
		Policy: keyauthority.KeyPolicy{
			DailyCostLimit: &keyDaily,
			RouteLimits:    keyauthority.RouteLimits{"chat": {DailyCostLimit: &routeDaily}},
			EndUserPolicy:  &keyauthority.EndUserPolicy{Defaults: keyauthority.AdmissionLimits{DailyCostLimit: &endUserKeyDaily}},
		},
	}
	result := authority.CostBoundaries("chat")
	if !result.Resolved || !result.CostBudgeted() {
		t.Fatalf("boundaries not resolved as cost budgeted: %+v", result)
	}
	type want struct {
		id                     string
		kind                   limits.OwnerKind
		basis                  limits.Basis
		daily, weekly, monthly *string
		level, subject, value  string
	}
	wants := []want{
		{id: authority.ID, kind: "key", daily: &keyDaily},
		{id: group, kind: "group", basis: limits.BasisCharge, monthly: &groupMonthly},
		{id: limits.KeyRouteBudgetID(authority.ID, "chat"), kind: "aggregate", daily: &routeDaily},
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
		if got.Budget == nil || got.Budget.Owner != got.Owner || got.Budget.Daily != expected.daily || got.Budget.Weekly != expected.weekly || got.Budget.Monthly != expected.monthly {
			t.Fatalf("boundary %d budget = %+v, want limits daily=%v weekly=%v monthly=%v", i, got.Budget, expected.daily, expected.weekly, expected.monthly)
		}
		aggregate := keyauthority.AggregateAccount{}
		if got.Aggregate != nil {
			aggregate = *got.Aggregate
		}
		if aggregate != (keyauthority.AggregateAccount{Level: expected.level, Key: expected.subject, Value: expected.value}) {
			t.Fatalf("boundary %d aggregate identity = %+v, want %q/%q/%q", i, aggregate, expected.level, expected.subject, expected.value)
		}
	}
	if owners, admissions := result.Owners(), result.Admissions(); len(owners) != len(wants) || len(admissions) != len(wants) {
		t.Fatalf("paired views disagree: owners=%+v admissions=%+v", owners, admissions)
	}
	if routeless := authority.CostBoundaries(""); len(routeless.Boundaries) != len(wants)-1 {
		t.Fatalf("a call without a route kept a route budget: %+v", routeless.Boundaries)
	}
}

func TestCostBoundariesUncappedKeyHasNoCeiling(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	authority := keyauthority.Authority{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603", LookupID: "lookup-one", ProjectID: &project}
	result := authority.CostBoundaries("")
	if !result.Resolved || result.CostBudgeted() {
		t.Fatalf("uncapped authority reported a ceiling: %+v", result)
	}
	if len(result.Boundaries) != 1 || result.Boundaries[0].Owner.Kind != "key" {
		t.Fatalf("uncapped authority gained boundaries: %+v", result.Boundaries)
	}
	limited := "5"
	authority.OrganizationBudget = &keyauthority.BudgetPolicy{DailyCostLimit: &limited}
	if authority.CostBoundaries("").Resolved {
		t.Fatal("aggregate boundary without a subject resolved")
	}
}

func TestDelegatedToolDebitEnforcesAgentPairAndAttributionBudgets(t *testing.T) {
	project, pair := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	agentDaily, pairWeekly, teamMonthly := "3", "4", "5"
	agentID, keyID := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50602", "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50603"
	authority := keyauthority.Authority{
		ID: keyID, ProjectID: &project, Attribution: map[string]string{"team": "core"}, DelegationPairDigest: pair,
		DelegationAgentLimits: &keyauthority.DelegationAgentLimits{ID: agentID, LookupID: "agent", Limits: keyauthority.AdmissionLimits{DailyCostLimit: &agentDaily}},
		ProjectAttributionBudgets: keyauthority.AttributionBudgets{
			attribution.DelegationPairKey: {pair: {WeeklyCostLimit: &pairWeekly}},
			"team":                        {"core": {MonthlyCostLimit: &teamMonthly}},
		},
	}
	result := authority.CostBoundaries("")
	if !result.Resolved || !result.CostBudgeted() {
		t.Fatalf("delegated budgets not available: %+v", result)
	}
	checks := []struct {
		owner                  string
		daily, weekly, monthly *string
	}{
		{agentID, &agentDaily, nil, nil},
		{limits.AttributionBudgetID(project, attribution.DelegationPairKey, pair), nil, &pairWeekly, nil},
		{limits.AttributionBudgetID(project, "team", "core"), nil, nil, &teamMonthly},
	}
	for _, check := range checks {
		index := slices.IndexFunc(result.Boundaries, func(boundary keyauthority.CostBoundary) bool { return boundary.Owner.ID == check.owner })
		if index < 0 || result.Boundaries[index].Budget == nil {
			t.Fatalf("missing delegated budget owner %s", check.owner)
		}
		budget := result.Boundaries[index].Budget
		equal := func(actual, expected *string) bool {
			if actual == nil || expected == nil {
				return actual == nil && expected == nil
			}
			return *actual == *expected
		}
		if !equal(budget.Daily, check.daily) || !equal(budget.Weekly, check.weekly) || !equal(budget.Monthly, check.monthly) {
			t.Fatalf("wrong limits for owner %s: %+v", check.owner, budget)
		}
	}
	uncapped := keyauthority.Authority{ID: keyID, ProjectID: &project, DelegationPairDigest: pair, DelegationAgentLimits: &keyauthority.DelegationAgentLimits{ID: agentID}}
	if uncapped.CostBoundaries("").CostBudgeted() {
		t.Fatal("tracking an uncapped delegated pair must not create a cost limit")
	}
}

func TestDelegatedAuthorityPreservesRestrictionsAndDropsGroupRoutes(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	expires := time.Now().Add(time.Hour)
	parent := keyauthority.Authority{
		ID:            "key-parent",
		ProjectID:     &project,
		SecretOverlap: &keyauthority.SecretOverlap{LookupID: "old", ExpiresAt: expires},
		Policy: keyauthority.KeyPolicy{
			Scopes:                  []string{"inference"},
			AllowedRoutes:           []string{"allowed"},
			AllowedRouteGroups:      []string{"group-a"},
			AllowedCIDRs:            []string{"10.0.0.0/8"},
			RequiredAttributionKeys: []string{"team"},
			ForbidResponseCache:     false,
		},
	}
	if err := parent.BindRouteGroups(keyauthority.RouteGroups{"group-a": {"group-only"}}); err != nil {
		t.Fatal(err)
	}
	agent := keyauthority.Authority{
		ID:     "agent-1",
		Policy: keyauthority.KeyPolicy{ForbidResponseCache: true},
	}
	delegated := parent.Delegated(keyauthority.DelegationDerivation{
		Scopes:      []string{"inference"},
		Routes:      []string{"allowed"},
		Agent:       agent,
		PairDigest:  "pair",
		TokenDigest: "token",
		ExpiresAt:   expires,
	})
	if delegated.SecretOverlap != nil {
		t.Fatal("delegated authority retained parent SecretOverlap")
	}
	if delegated.Allows("inference", "group-only", &project, time.Now()) {
		t.Fatal("delegated authority widened past explicit routes via parent route groups")
	}
	if !slices.Equal(delegated.Policy.AllowedCIDRs, []string{"10.0.0.0/8"}) ||
		!slices.Equal(delegated.Policy.RequiredAttributionKeys, []string{"team"}) ||
		!delegated.Policy.ForbidResponseCache {
		t.Fatalf("delegated authority lost parent or agent restrictions: %+v", delegated.Policy)
	}
}

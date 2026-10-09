package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestUnboundedWorkRejectsEveryConsumptionBudget(t *testing.T) {
	for _, name := range []string{
		"key tokens", "key daily", "key weekly", "key monthly", "group tokens", "group daily", "group weekly", "group monthly",
		"key route tokens", "key route cost", "key end user tokens", "key end user cost", "project end user tokens", "project end user cost",
		"installation", "organization", "project", "attribution", "route daily", "route monthly",
		"provider tokens", "provider daily", "provider monthly", "slot tokens", "slot daily", "slot monthly",
	} {
		t.Run(name, func(t *testing.T) {
			n, cost, group := int64(100), "1.00", benchOwner
			a := access.Authority{BudgetGroupID: &group, EndUserDigest: "user", Attribution: map[string]string{"team": "core"}}
			p := runtime.Provider{Limits: &runtime.Limits{}}
			s := runtime.Slot{}
			route := runtime.Route{Slug: "chat"}
			switch name {
			case "key tokens":
				a.Policy.TokensPerMinute = &n
			case "key daily":
				a.Policy.DailyCostLimit = &cost
			case "key weekly":
				a.Policy.WeeklyCostLimit = &cost
			case "key monthly":
				a.Policy.MonthlyCostLimit = &cost
			case "group tokens":
				a.BudgetGroupTPM = &n
			case "group daily":
				a.BudgetGroupDailyCostLimit = &cost
			case "group weekly":
				a.BudgetGroupWeeklyCostLimit = &cost
			case "group monthly":
				a.BudgetGroupMonthlyCostLimit = &cost
			case "key route tokens":
				a.Policy.RouteLimits = access.RouteLimits{"chat": {TokensPerMinute: &n}}
			case "key route cost":
				a.Policy.RouteLimits = access.RouteLimits{"chat": {WeeklyCostLimit: &cost}}
			case "key end user tokens", "project end user tokens", "key end user cost", "project end user cost":
				limit := access.AdmissionLimits{TokensPerMinute: &n}
				if name == "key end user cost" || name == "project end user cost" {
					limit = access.AdmissionLimits{WeeklyCostLimit: &cost}
				}
				policy := &access.EndUserPolicy{Overrides: map[string]access.AdmissionLimits{"user": limit}}
				if name == "key end user tokens" || name == "key end user cost" {
					a.Policy.EndUserPolicy = policy
				} else {
					a.ProjectEndUserPolicy = policy
				}
			case "installation":
				a.InstallationBudget = &access.BudgetPolicy{WeeklyCostLimit: &cost}
			case "organization":
				a.OrganizationBudget = &access.BudgetPolicy{WeeklyCostLimit: &cost}
			case "project":
				a.ProjectBudget = &access.BudgetPolicy{WeeklyCostLimit: &cost}
			case "attribution":
				a.ProjectAttributionBudgets = access.AttributionBudgets{"team": {"core": {WeeklyCostLimit: &cost}}}
			case "route daily":
				route.Budget = &runtime.CostLimits{DailyCostLimit: &cost}
			case "route monthly":
				route.Budget = &runtime.CostLimits{MonthlyCostLimit: &cost}
			case "provider tokens":
				p.Limits.TokensPerMinute = &n
			case "provider daily":
				p.Limits.DailyCostLimit = &cost
			case "provider monthly":
				p.Limits.MonthlyCostLimit = &cost
			case "slot tokens":
				s.TokensPerMinute = &n
			case "slot daily":
				s.DailyCostLimit = &cost
			case "slot monthly":
				s.MonthlyCostLimit = &cost
			}
			if e := unboundedWorkLimits(a, &p, &s, &route); e == nil || e.Code != "unbounded_work_budget" {
				t.Fatalf("consumption budget admitted: %v", e)
			}
		})
	}
}

func TestUnboundedWorkKeepsEnforceableLimitsAndUnrelatedBudgets(t *testing.T) {
	n, cost, id := int64(100), "1", benchOwner
	policy := access.AdmissionLimits{RequestsPerMinute: &n, MaxConcurrency: &n}
	a := access.Authority{BudgetGroupID: &id, BudgetGroupRPM: &n, BudgetGroupConcurrency: &n, EndUserDigest: "current"}
	a.Policy.RequestsPerMinute, a.Policy.MaxConcurrency = &n, &n
	a.Policy.RouteLimits = access.RouteLimits{"chat": policy, "other": {TokensPerMinute: &n, WeeklyCostLimit: &cost}}
	a.Policy.EndUserPolicy = &access.EndUserPolicy{Defaults: policy, Overrides: map[string]access.AdmissionLimits{"other": {WeeklyCostLimit: &cost}}}
	a.ProjectAttributionBudgets = access.AttributionBudgets{"team": {"other": {WeeklyCostLimit: &cost}}}
	a.Attribution = map[string]string{"team": "current"}
	p := runtime.Provider{Limits: &runtime.Limits{RequestsPerMinute: &n, MaxConcurrency: &n}}
	s := runtime.Slot{RequestsPerMinute: &n, MaxConcurrency: &n}
	if e := unboundedWorkLimits(a, &p, &s, &runtime.Route{Slug: "chat", Budget: &runtime.CostLimits{}}); e != nil {
		t.Fatalf("enforceable session refused: %v", e)
	}
	if e := unboundedWorkLimits(access.Authority{}, nil, nil, nil); e != nil {
		t.Fatal(e)
	}
}

func TestUnboundedPinObservesCurrentRouteProviderAndSlotBudgets(t *testing.T) {
	for _, scope := range []string{"route", "provider", "slot"} {
		t.Run(scope, func(t *testing.T) {
			provider := runtime.Provider{ID: "provider", Slots: []runtime.Slot{{ID: "slot"}}}
			route := runtime.Route{Slug: "chat"}
			p := &pin{provider: provider, slot: provider.Slots[0]}
			snapshot := &runtime.Snapshot{Routes: map[string]runtime.Route{"chat": route}, Providers: map[string]runtime.Provider{"provider": provider}}
			s := &Server{Runtime: &fakeRuntime{release: &runtime.Release{Snapshot: snapshot}}}
			if e := s.unboundedPinLimits(access.Authority{}, p, &route); e != nil {
				t.Fatal(e)
			}
			cost := "1"
			switch scope {
			case "route":
				current := route
				current.Budget = &runtime.CostLimits{MonthlyCostLimit: &cost}
				snapshot.Routes["chat"] = current
			case "provider":
				provider.Limits = &runtime.Limits{Supply: runtime.Supply{DailyCostLimit: &cost}}
				snapshot.Providers[provider.ID] = provider
			case "slot":
				provider.Slots = []runtime.Slot{{ID: "slot", Supply: runtime.Supply{DailyCostLimit: &cost}}}
				snapshot.Providers[provider.ID] = provider
			}
			if e := s.unboundedPinLimits(access.Authority{}, p, &route); e == nil || e.Code != "unbounded_work_budget" {
				t.Fatalf("current %s cap bypassed by retained pin: %v", scope, e)
			}
		})
	}
}

func TestCostAccountingRefusesEveryCallerBudgetBeforeReservation(t *testing.T) {
	for _, scope := range []string{"key", "group", "key route", "key end user", "project end user", "installation", "organization", "project", "attribution"} {
		t.Run(scope, func(t *testing.T) {
			cost, id := "1", benchOwner
			authority := access.Authority{ID: benchOwner, LookupID: "lookup_test", EndUserDigest: "user", Attribution: map[string]string{"team": "core"}}
			budget := &access.BudgetPolicy{WeeklyCostLimit: &cost}
			switch scope {
			case "key":
				authority.Policy.WeeklyCostLimit = &cost
			case "group":
				authority.BudgetGroupID, authority.BudgetGroupWeeklyCostLimit = &id, &cost
			case "key route":
				authority.Policy.RouteLimits = access.RouteLimits{"chat": {WeeklyCostLimit: &cost}}
			case "key end user":
				authority.Policy.EndUserPolicy = &access.EndUserPolicy{Defaults: access.AdmissionLimits{WeeklyCostLimit: &cost}}
			case "project end user":
				authority.ProjectEndUserPolicy = &access.EndUserPolicy{Defaults: access.AdmissionLimits{WeeklyCostLimit: &cost}}
			case "installation":
				authority.InstallationBudget = budget
			case "organization":
				authority.OrganizationBudget = budget
			case "project":
				authority.ProjectBudget = budget
			case "attribution":
				authority.ProjectAttributionBudgets = access.AttributionBudgets{"team": {"core": *budget}}
			}
			checked := 0
			a := &Admission{CostAccountingReady: func(ctx context.Context) error {
				checked++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("accounting check is unbounded")
				}
				return errors.New("lost")
			}}
			lease, e := a.reserveKey(t.Context(), authority, "openai", 100, time.Minute, "chat")
			if checked != 1 || lease != nil || e == nil || e.Code != "cost_accounting_incomplete" {
				t.Fatalf("accounting loss bypassed: checks=%d lease=%v refusal=%v", checked, lease, e)
			}
		})
	}
}

func TestCostAccountingSkipsUnlimitedAndRateOnlyAdmission(t *testing.T) {
	for _, rate := range []bool{false, true} {
		authority := access.Authority{ID: benchOwner, LookupID: "lookup_test"}
		var requests int64
		if rate {
			requests = 600
			authority.Policy.RequestsPerMinute = &requests
		}
		authority.Policy.RouteLimits = access.RouteLimits{"other": {WeeklyCostLimit: stringptr("1")}}
		client := newAllowing(requests, 0, 100, false)
		a := newAdmission(t, client)
		a.CostAccountingReady = func(context.Context) error { t.Fatal("non-cost admission checked accounting"); return nil }
		if _, e := a.reserveKey(t.Context(), authority, "openai", 100, time.Minute, "chat"); e != nil {
			t.Fatal(e)
		}
		if !rate && client.calls.Load() != 0 {
			t.Fatal("unlimited admission reached shared state")
		}
	}
}

func TestHealthyAccountingIsCheckedOnceBeforeKeyAndGroupReservation(t *testing.T) {
	group, cost := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608", "1"
	authority := access.Authority{ID: benchOwner, LookupID: "lookup_test", BudgetGroupID: &group, BudgetGroupMonthlyCostLimit: &cost}
	authority.Policy.DailyCostLimit = &cost
	client := newAllowing(0, 0, 100, false)
	a := newAdmission(t, client)
	checks := 0
	a.CostAccountingReady = func(context.Context) error {
		checks++
		if client.calls.Load() != 0 {
			t.Fatal("cost was reserved before checking accounting")
		}
		return nil
	}
	lease, e := a.reserveKeyCosted(t.Context(), &authority, "openai", 100, time.Minute, costReservation{amount: "0.01", requestID: benchOwner})
	if e != nil || lease == nil || !lease.HasCostReservation() || checks != 1 || client.calls.Load() != 2 {
		t.Fatalf("key/group admission: lease=%v refusal=%v checks=%d reservations=%d", lease, e, checks, client.calls.Load())
	}
}

type budgetRuntime struct {
	*fakeRuntime
	authority access.Authority
}

func (r budgetRuntime) BudgetAuthority(*string) (access.Authority, error) { return r.authority, nil }

func TestSystemCostAdmissionRefusesIncompleteAccounting(t *testing.T) {
	for _, scope := range []string{"installation", "organization", "project", "attribution"} {
		t.Run(scope, func(t *testing.T) {
			cost := "1"
			authority := access.Authority{}
			budget := &access.BudgetPolicy{WeeklyCostLimit: &cost}
			switch scope {
			case "installation":
				authority.InstallationBudget = budget
			case "organization":
				authority.OrganizationBudget = budget
			case "project":
				authority.ProjectBudget = budget
			case "attribution":
				authority.ProjectAttributionBudgets = access.AttributionBudgets{"team": {"core": *budget}}
			}
			checked := false
			s := &Server{Runtime: budgetRuntime{authority: authority}, Admission: &Admission{CostAccountingReady: func(context.Context) error { checked = true; return errors.New("lost") }}}
			x := &execution{route: &runtime.Route{Slug: "chat"}, attribution: map[string]string{"team": "core"}}
			if e := s.reserveSystemBudgets(t.Context(), x); !checked || e == nil || e.Code != "cost_accounting_incomplete" {
				t.Fatalf("system budget admitted against lost accounting: checked=%v refusal=%v", checked, e)
			}
		})
	}
}

func TestSupplyCostAdmissionRefusesIncompleteAccountingBeforeReserving(t *testing.T) {
	for _, scope := range []string{quotaRoute, quotaConnection, quotaSlot} {
		t.Run(scope, func(t *testing.T) {
			cost := "1"
			x := &execution{route: &runtime.Route{Slug: "chat"}}
			provider, slot := runtime.Provider{}, runtime.Slot{}
			switch scope {
			case quotaRoute:
				x.route.Budget = &runtime.CostLimits{DailyCostLimit: &cost}
			case quotaConnection:
				provider.Limits = &runtime.Limits{Supply: runtime.Supply{MonthlyCostLimit: &cost}}
			case quotaSlot:
				slot.DailyCostLimit = &cost
			}
			client := newAllowing(0, 0, 100, false)
			a := newAdmission(t, client)
			a.CostAccountingReady = func(context.Context) error { return errors.New("lost") }
			s := &Server{Admission: a}
			check := s.holdCaps(t.Context(), x, runtime.Attempt{}, &provider, &slot, time.Now().Add(time.Minute))
			if check.quota != scope || client.calls.Load() != 0 {
				t.Fatalf("supply cap admitted: %+v calls=%d", check, client.calls.Load())
			}
		})
	}
}

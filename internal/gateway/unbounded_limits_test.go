package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestUnboundedPinObservesCurrentRouteProviderAndSlotBudgets(t *testing.T) {
	for _, scope := range []string{"route", "provider", "slot"} {
		t.Run(scope, func(t *testing.T) {
			provider := routing.Provider{ID: "provider", Slots: []routing.Slot{{ID: "slot"}}}
			route := routing.Route{Slug: "chat"}
			p := &pin{provider: provider, slot: provider.Slots[0]}
			snapshot := &routing.Snapshot{Routes: map[string]routing.Route{"chat": route}, Providers: map[string]routing.Provider{"provider": provider}}
			s := &Server{Runtime: &fakeRuntime{release: &runtime.Release{Snapshot: snapshot}}}
			if e := s.unboundedPinLimits(access.Authority{}, p, &route); e != nil {
				t.Fatal(e)
			}
			cost := "1"
			switch scope {
			case "route":
				current := route
				current.Budget = &routing.CostLimits{MonthlyCostLimit: &cost}
				snapshot.Routes["chat"] = current
			case "provider":
				provider.Limits = &routing.Limits{Supply: routing.Supply{DailyCostLimit: &cost}}
				snapshot.Providers[provider.ID] = provider
			case "slot":
				provider.Slots = []routing.Slot{{ID: "slot", Supply: routing.Supply{DailyCostLimit: &cost}}}
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

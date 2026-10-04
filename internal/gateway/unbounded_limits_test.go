package gateway

import (
	"context"
	"errors"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
	"testing"
	"time"
)

func TestUnboundedWorkRejectsEveryConsumptionBudget(t *testing.T) {
	n := int64(100)
	cost := "1.00"
	for _, name := range []string{"key tokens", "key daily", "key monthly", "group daily", "group monthly", "provider tokens", "slot tokens"} {
		t.Run(name, func(t *testing.T) {
			a := access.Authority{}
			p := runtime.Provider{}
			s := runtime.Slot{}
			switch name {
			case "key tokens":
				a.Policy.TokensPerMinute = &n
			case "key daily":
				a.Policy.DailyCostLimit = &cost
			case "key monthly":
				a.Policy.MonthlyCostLimit = &cost
			case "group daily":
				a.BudgetGroupDailyCostLimit = &cost
			case "group monthly":
				a.BudgetGroupMonthlyCostLimit = &cost
			case "provider tokens":
				p.Limits = &runtime.Limits{TokensPerMinute: &n}
			case "slot tokens":
				s.TokensPerMinute = &n
			}
			if e := unboundedWorkLimits(a, &p, &s); e == nil || e.Code != "unbounded_work_budget" {
				t.Fatalf("budget admitted: %v", e)
			}
		})
	}
	a := access.Authority{}
	a.Policy.RequestsPerMinute, a.Policy.MaxConcurrency = &n, &n
	p := runtime.Provider{Limits: &runtime.Limits{RequestsPerMinute: &n, MaxConcurrency: &n}}
	s := runtime.Slot{RequestsPerMinute: &n, MaxConcurrency: &n}
	if e := unboundedWorkLimits(a, &p, &s); e != nil {
		t.Fatalf("enforceable limits refused: %v", e)
	}
	if e := unboundedWorkLimits(access.Authority{}, nil, nil); e != nil {
		t.Fatal(e)
	}
}

func TestCostAdmissionFailsClosedOnAccountingLoss(t *testing.T) {
	cost := "1.00"
	for _, group := range []bool{false, true} {
		authority := access.Authority{}
		if group {
			id := "group"
			authority.BudgetGroupID, authority.BudgetGroupDailyCostLimit = &id, &cost
		} else {
			authority.Policy.DailyCostLimit = &cost
		}
		checked := false
		a := &Admission{CostAccountingReady: func(context.Context) error { checked = true; return errors.New("lost") }}
		lease, e := a.reserveKey(t.Context(), authority, "openai", 100, time.Minute)
		if !checked || lease != nil || e == nil || e.Code != "cost_accounting_incomplete" {
			t.Fatalf("lost accounting admitted: %v %v %v", checked, lease, e)
		}
	}
}

func TestCostAccountingChecksOnlyBudgetedAdmissionBeforeReservation(t *testing.T) {
	for _, tc := range []struct {
		name             string
		key, group, rate bool
		wantChecks       int
	}{
		{name: "unlimited"},
		{name: "rate only", rate: true},
		{name: "key budget", key: true, wantChecks: 1},
		{name: "group budget", group: true, wantChecks: 1},
		{name: "both budgets", key: true, group: true, wantChecks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority := access.Authority{ID: benchOwner, LookupID: "lookup_test"}
			var requests int64
			if tc.rate {
				requests = 600
				authority.Policy.RequestsPerMinute = &requests
			}
			if tc.key {
				authority.Policy.DailyCostLimit = stringptr("1.00")
			}
			if tc.group {
				id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
				authority.BudgetGroupID = &id
				authority.BudgetGroupMonthlyCostLimit = stringptr("1.00")
			}
			client := newAllowing(requests, 0, 100, false)
			admission := newAdmission(t, client)
			checks := 0
			admission.CostAccountingReady = func(ctx context.Context) error {
				checks++
				if client.calls.Load() != 0 {
					t.Fatal("reserved cost before checking accounting")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("accounting check has no deadline")
				}
				return nil
			}
			_, e := admission.reserveKeyCosted(t.Context(), authority, "openai", 100, time.Minute, costReservation{amount: "0.01", requestID: benchOwner})
			if e != nil {
				t.Fatal(e)
			}
			if checks != tc.wantChecks {
				t.Fatalf("accounting checks=%d, want%d", checks, tc.wantChecks)
			}
		})
	}
}

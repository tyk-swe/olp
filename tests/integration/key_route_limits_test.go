//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

func TestKeyRouteBudgetsPreserveSpendAndRemainKeyScoped(t *testing.T) {
	for _, field := range []string{"daily_cost_limit", "weekly_cost_limit", "monthly_cost_limit"} {
		t.Run(field, func(t *testing.T) { testKeyRouteBudget(t, field) })
	}
}
func testKeyRouteBudget(t *testing.T, field string) {
	f := glSeedIn(t, "key-route-budget", glPrice{input: "1000000", output: "1000000"})
	id, secret := f.key("route-budget", nil)
	_, other := f.key("other-key", nil)
	await := endUserAccounting(t, f.h, f.limiter)
	call := func(token string, want int) {
		t.Helper()
		status, err := endUserChat(context.Background(), f.h, token, routeSlug, "customer")
		if err != nil || status != want {
			t.Fatalf("status=%d want=%d err=%v", status, want, err)
		}
		await()
	}
	call(secret, 200)
	path := "/api/v1/api-keys/" + id
	update := func(policy any) {
		t.Helper()
		detail := f.h.want(f.owner, "GET", path, nil, nil, 200)
		f.h.want(f.owner, "PATCH", path, map[string]any{"route_limits": policy}, etagHeader(detail), 200)
		f.h.refresh()
	}
	update(map[string]any{routeSlug: map[string]any{field: "1000"}})
	call(secret, 503)
	call(secret, 200)
	var cost string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT accrued::text FROM olp.aggregate_cost_windows WHERE account_id=$1 AND window_kind='day'`, limits.KeyRouteBudgetID(id, routeSlug)).Scan(&cost); err != nil || cost != "20.000000000000" {
		t.Fatalf("cost=%s err=%v", cost, err)
	}
	update(map[string]any{routeSlug: map[string]any{field: "20"}})
	call(secret, 429)
	call(other, 200)
	update(nil)
	call(secret, 200)
	update(map[string]any{routeSlug: map[string]any{field: "20"}})
	call(secret, 429)
}

func TestKeyRouteRateLimitsAcrossGateways(t *testing.T) {
	f := glSeedIn(t, "key-route-rate", glPrice{})
	_, secret := f.key("route-rpm", map[string]any{"route_limits": map[string]any{routeSlug: map[string]any{"requests_per_minute": 2}, "other-route": map[string]any{"requests_per_minute": 1}}})
	replica := newAccessHarnessOn(t, f.h.Pool, f.h.DBURL)
	replica.Gateway.Admission = f.h.Gateway.Admission
	replica.refresh()
	limSettleInMinute(t, f.valkey, 5*time.Second)
	for i, want := range []int{200, 200, 429} {
		h := f.h
		if i%2 == 1 {
			h = replica
		}
		status, err := endUserChat(context.Background(), h, secret, routeSlug, "customer")
		if err != nil || status != want {
			t.Fatalf("request %d status=%d want=%d err=%v", i, status, want, err)
		}
	}
	authority, err := f.h.Runtime.Authenticate(secret)
	if err != nil {
		t.Fatal(err)
	}
	lease, refusal := f.h.Gateway.Admission.ReserveCodeRate(t.Context(), *authority, 1, time.Minute, "other-route")
	if refusal != nil {
		t.Fatalf("independent route refused: %v", refusal)
	}
	if err = lease.Refund(t.Context()); err != nil {
		t.Fatal(err)
	}

}

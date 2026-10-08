//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestTemporaryBudgetIncreasesExpireWithoutAuthorityRefresh(t *testing.T) {
	for _, window := range []string{"day", "week", "month"} {
		t.Run(window, func(t *testing.T) {
			field := map[string]string{"day": "daily_cost_limit", "week": "weekly_cost_limit", "month": "monthly_cost_limit"}[window]
			f := glSeedIn(t, "increase-"+window, glPrice{input: "1000000", output: "1000000"})
			h := f.h
			id, secret := f.key("temporary", map[string]any{field: "1"})
			await := endUserAccounting(t, h, f.limiter)
			if status, err := endUserChat(t.Context(), h, secret, routeSlug, "unused"); err != nil || status != 503 {
				t.Fatalf("initialize %d %v", status, err)
			}
			await()
			conn, err := h.Pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Now())
			conn.Release()
			if err != nil {
				t.Fatal(err)
			}
			for _, snapshot := range snapshots {
				if _, _, err = f.limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
					t.Fatal(err)
				}
			}
			replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
			replica.Gateway.Admission = gateway.NewAdmission(f.limiter, nil, slog.New(slog.DiscardHandler))
			replica.Gateway.Sink = h.Gateway.Sink
			expiry := time.Now().UTC().Add(4 * time.Second)
			body := map[string]any{"target": map[string]any{"kind": "api_key", "id": id}, "window": window, "amount": "100", "reason": "Temporary incident capacity", "expires_at": expiry.Format(time.RFC3339Nano)}
			replay := idem("increase")
			grant := h.want(f.owner, "POST", "/api/v1/budget-increases", body, replay, 201)
			again := h.want(f.owner, "POST", "/api/v1/budget-increases", body, replay, 201)
			if grant["id"] != again["id"] || grant["expires_at"] != again["expires_at"] {
				t.Fatal("replay renewed increase")
			}
			h.refresh()
			replica.refresh()
			detail := h.want(f.owner, "GET", "/api/v1/api-keys/"+id, nil, nil, 200)
			period := map[string]string{"day": "daily", "week": "weekly", "month": "monthly"}[window]
			report := detail["budget"].(map[string]any)[period].(map[string]any)
			if report["limit"] != "1" || report["effective_limit"] != "101.000000000000" {
				t.Fatalf("policy/allowance mixed: %v", report)
			}
			if status, err := endUserChat(t.Context(), h, secret, routeSlug, "unused"); err != nil || status != 200 {
				t.Fatalf("increase %d %v", status, err)
			}
			await()
			if status, err := endUserChat(t.Context(), replica, secret, routeSlug, "unused"); err != nil || status != 200 {
				t.Fatalf("replica increase: %d %v", status, err)
			}
			await()
			deadline, err := time.Parse(time.RFC3339Nano, grant["expires_at"].(string))
			if err != nil {
				t.Fatal(err)
			}
			timer := time.NewTimer(time.Until(deadline) + 20*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-t.Context().Done():
				t.Fatal("expired wait cancelled")
			}
			if status, err := endUserChat(t.Context(), h, secret, routeSlug, "unused"); err != nil || status != 429 {
				t.Fatalf("cached grant survived expiry: %d %v", status, err)
			}
			await()
			// A new increase adds no counter reset; explicit revocation restores the cap.
			delete(body, "expires_at")
			grant = h.want(f.owner, "POST", "/api/v1/budget-increases", body, idem("new-increase"), 201)
			h.want(f.owner, "DELETE", "/api/v1/budget-increases/"+grant["id"].(string), nil, etagHeader(grant), 204)
			h.refresh()
			if status, err := endUserChat(t.Context(), h, secret, routeSlug, "unused"); err != nil || status != 429 {
				t.Fatalf("revoked increase: %d %v", status, err)
			}
			await()
			var count int
			if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='budget.increase.create'").Scan(&count); err != nil || count != 2 {
				t.Fatalf("audit %d %v", count, err)
			}
		})
	}
}

func TestBudgetIncreaseWindowIdentityAndExactAddition(t *testing.T) {
	client := limClient(t)
	limiter := limLimiter(t, client, limNamespace(t, client, "increase-evidence"))
	w := limits.BudgetWindows(time.Now())
	owner := uuid.NewString()
	s := limits.CostSnapshot{CostOwnerID: owner, DailyWindowID: w.DailyID, DailyAccrued: "0.000000000002", WeeklyWindowID: w.WeeklyID, WeeklyAccrued: "0", MonthlyWindowID: w.MonthlyID, MonthlyAccrued: "0"}
	if _, _, err := limiter.ApplyCostSnapshot(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	r := limRequest(limLookup())
	r.CostOwnerID = owner
	r.DailyCostLimit = limPointer("0.000000000001")
	grant := map[string]any{"window": "day", "window_id": w.DailyID - 1, "amount": "0.000000000002", "starts_at": time.Now().Add(-time.Hour).UnixMilli(), "expires_at": time.Now().Add(time.Hour).UnixMilli()}
	encode := func() { b, _ := json.Marshal([]any{grant}); r.CostIncreases = string(b) }
	encode()
	var exceeded *limits.ExceededError
	if _, err := limiter.Reserve(t.Context(), r); !errors.As(err, &exceeded) {
		t.Fatalf("wrong-period grant: %v", err)
	}
	grant["window_id"] = w.DailyID
	encode()
	lease, err := limiter.Reserve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Refund(t.Context()); err != nil {
		t.Fatal(err)
	}
	grant["expires_at"] = time.Now().Add(-time.Minute).UnixMilli()
	encode()
	if _, err = limiter.Reserve(t.Context(), r); !errors.As(err, &exceeded) {
		t.Fatalf("expired grant: %v", err)
	}
}

func TestBudgetIncreaseBoundsTargetsAndScopes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	p := createProject(h, owner, "increase-project")
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "key", "project_id": p, "scopes": []string{"inference"}, "daily_cost_limit": "5", "route_limits": map[string]any{"future": map[string]any{"daily_cost_limit": "3"}}, "end_user_source": "header", "end_user_policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": "4"}}}, idem("key"), 201)
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "group", "project_id": p, "daily_cost_limit": "2"}, idem("group"), 201)
	for path, policy := range map[string]any{"budget": map[string]any{"daily_cost_limit": "1"}, "end-user-policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": "1"}}} {
		detail := h.want(owner, "GET", "/api/v1/projects/"+p+"/"+path, nil, nil, 200)
		h.want(owner, "PUT", "/api/v1/projects/"+p+"/"+path, map[string]any{"policy": policy}, etagHeader(detail), 200)
	}
	labels := h.want(owner, "GET", "/api/v1/projects/"+p+"/attribution-budgets", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/projects/"+p+"/attribution-budgets", map[string]any{"budgets": map[string]any{"team": map[string]any{"core": map[string]any{"daily_cost_limit": "1"}}}}, etagHeader(labels), 200)
	installation := h.want(owner, "GET", "/api/v1/budgets/installation", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/budgets/installation", map[string]any{"policy": map[string]any{"daily_cost_limit": "1"}}, etagHeader(installation), 200)
	targets := []map[string]any{{"kind": "api_key", "id": key["id"]}, {"kind": "budget_group", "id": group["id"]}, {"kind": "project", "id": p}, {"kind": "installation"}, {"kind": "key_route", "id": key["id"], "route": "future"}, {"kind": "key_end_user", "id": key["id"], "end_user_digest": strings.Repeat("a", 64)}, {"kind": "project_end_user", "id": p, "end_user_digest": strings.Repeat("a", 64)}, {"kind": "attribution", "id": p, "label": "team", "value": "core"}}
	for i, target := range targets {
		input := map[string]any{"target": target, "window": "day", "amount": "1", "reason": "Planned capacity", "expires_at": time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339)}
		grant := h.want(owner, "POST", "/api/v1/budget-increases", input, idem(fmt.Sprint(i)), 201)
		if grant["expires_at"] != grant["window_ends_at"] {
			t.Fatalf("escaped window: %v", grant)
		}
		h.want(owner, "GET", "/api/v1/budget-increases/"+grant["id"].(string), nil, nil, 200)
	}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "keys only", "scopes": []string{"keys"}, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	h.machineWant(token, "POST", "/api/v1/budget-increases", map[string]any{"target": map[string]any{"kind": "installation"}, "window": "day", "amount": "1", "reason": "Denied"}, idem("denied"), 403)
	input := map[string]any{"target": targets[0], "window": "day", "amount": "1", "reason": "Bound"}
	for i := 0; i < 7; i++ {
		h.want(owner, "POST", "/api/v1/budget-increases", input, idem(fmt.Sprint("bound", i)), 201)
	}
	h.want(owner, "POST", "/api/v1/budget-increases", input, idem("ninth"), 409)
	for _, bad := range []map[string]any{{"amount": "0"}, {"amount": "1e3"}, {"reason": ""}, {"reason": "bad\nline"}, {"window": "year"}, {"expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}, {"target": map[string]any{"kind": "api_key", "id": key["id"], "label": "team"}}} {
		b := maps.Clone(input)
		for k, v := range bad {
			b[k] = v
		}
		h.want(owner, "POST", "/api/v1/budget-increases", b, idem(uuid.NewString()), 422)
	}
}

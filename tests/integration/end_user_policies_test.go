//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

func endUserChat(ctx context.Context, h *accessHarness, secret, slug, identifier string) (int, error) {
	return endUserChatMode(ctx, h, secret, slug, identifier, false)
}

func endUserChatMode(ctx context.Context, h *accessHarness, secret, slug, identifier string, stream bool) (int, error) {
	body, err := json.Marshal(map[string]any{"model": slug, "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "user": identifier, "max_tokens": 16, "stream": stream})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", h.HTTP.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, err
}

func TestEndUserPoliciesShareLimitsAcrossGateways(t *testing.T) {
	f := glSeedIn(t, "end-users", glPrice{})
	id, secret := f.key("identity", map[string]any{"end_user_source": "native", "end_user_policy": map[string]any{"defaults": map[string]any{"requests_per_minute": 7}}})
	replica := newAccessHarnessOn(t, f.h.Pool, f.h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), f.namespace), nil, slog.New(slog.DiscardHandler))
	replica.refresh()
	limSettleInMinute(t, f.valkey, 10*time.Second)
	var wg sync.WaitGroup
	results := make(chan int, 20)
	for i := range 20 {
		h := f.h
		if i%2 != 0 {
			h = replica
		}
		wg.Go(func() {
			status, err := endUserChat(t.Context(), h, secret, routeSlug, "customer-a")
			if err != nil {
				t.Error(err)
			}
			results <- status
		})
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[200] != 7 || counts[429] != 13 {
		t.Fatalf("shared end-user limit: %v", counts)
	}
	if status, err := endUserChat(t.Context(), replica, secret, routeSlug, "customer-b"); status != 200 || err != nil {
		t.Fatalf("distinct identity: %d %v", status, err)
	}
	digest := f.h.want(f.owner, "POST", "/api/v1/api-keys/"+id+"/end-user", map[string]any{"identifier": "customer-b"}, nil, 200)["end_user_digest"]
	detail := f.h.want(f.owner, "GET", "/api/v1/api-keys/"+id, nil, nil, 200)
	f.h.want(f.owner, "PATCH", "/api/v1/api-keys/"+id, map[string]any{"end_user_policy": map[string]any{"blocked": []any{digest}}}, etagHeader(detail), 200)
	replica.refresh()
	before := f.vendor.chats.Load()
	if status, err := endUserChat(t.Context(), replica, secret, routeSlug, "customer-b"); status != 403 || err != nil {
		t.Fatalf("blocked identity: %d %v", status, err)
	}
	if f.vendor.chats.Load() != before {
		t.Fatal("blocked identity reached provider")
	}
	// Complete overrides can remove a default budget, or replace a rate independently.
	overrides := map[string]any{}
	for _, name := range []string{"unlimited", "bounded"} {
		lookedUp := f.h.want(f.owner, "POST", "/api/v1/api-keys/"+id+"/end-user", map[string]any{"identifier": name}, nil, 200)
		limit := map[string]any{}
		if name == "bounded" {
			limit["requests_per_minute"] = 1
		}
		overrides[lookedUp["end_user_digest"].(string)] = limit
	}
	detail = f.h.want(f.owner, "GET", "/api/v1/api-keys/"+id, nil, nil, 200)
	f.h.want(f.owner, "PATCH", "/api/v1/api-keys/"+id, map[string]any{"end_user_policy": map[string]any{
		"defaults": map[string]any{"requests_per_minute": 7, "daily_cost_limit": "1"}, "overrides": overrides,
	}}, etagHeader(detail), 200)
	replica.refresh()
	limSettleInMinute(t, f.valkey, 5*time.Second)
	for _, scenario := range []struct {
		name string
		want []int
	}{{"unlimited", []int{200, 200}}, {"bounded", []int{200, 429}}} {
		for _, want := range scenario.want {
			if status, err := endUserChat(t.Context(), replica, secret, routeSlug, scenario.name); err != nil || status != want {
				t.Fatalf("override %s: got %d, want %d: %v", scenario.name, status, want, err)
			}
		}
	}

}

func TestEndUserConcurrencyIsIndependentPerIdentity(t *testing.T) {
	f := glSeedIn(t, "end-user-concurrency", glPrice{})
	_, secret := f.key("concurrent", map[string]any{"end_user_source": "native", "end_user_policy": map[string]any{"defaults": map[string]any{"max_concurrency": 1}}})
	replica := newAccessHarnessOn(t, f.h.Pool, f.h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), f.namespace), nil, slog.New(slog.DiscardHandler))
	replica.refresh()
	f.vendor.delay.Store(int64(400 * time.Millisecond))
	before := f.vendor.chats.Load()
	finished := make(chan error, 1)
	go func() {
		status, err := endUserChatMode(t.Context(), f.h, secret, routeSlug, "busy", true)
		if err == nil && status != 200 {
			err = fmt.Errorf("first request: %d", status)
		}
		finished <- err
	}()
	glEventually(t, "the first end-user request to dispatch", func() bool { return f.vendor.chats.Load() > before })
	if status, err := endUserChat(t.Context(), replica, secret, routeSlug, "busy"); status != 429 || err != nil {
		t.Fatalf("shared concurrency: %d %v", status, err)
	}
	if status, err := endUserChat(t.Context(), replica, secret, routeSlug, "independent"); status != 200 || err != nil {
		t.Fatalf("distinct identity: %d %v", status, err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if status, err := endUserChat(t.Context(), replica, secret, routeSlug, "busy"); status != 200 || err != nil {
		t.Fatalf("released concurrency: %d %v", status, err)
	}
}

func TestEndUserTokenReservationsReconcileToObservedUsage(t *testing.T) {
	f := glSeedIn(t, "end-user-tokens", glPrice{})
	id, secret := f.key("tokens", map[string]any{"end_user_source": "native", "end_user_policy": map[string]any{"defaults": map[string]any{"tokens_per_minute": 1000}}})
	limSettleInMinute(t, f.valkey, 10*time.Second)
	for _, step := range []struct {
		user   string
		tokens string
	}{{"one", "10"}, {"two", "10"}, {"one", "20"}} {
		digest := f.h.want(f.owner, "POST", "/api/v1/api-keys/"+id+"/end-user", map[string]any{"identifier": step.user}, nil, 200)["end_user_digest"].(string)
		key, _ := limRateKeys(f.namespace, limits.EndUserLookup(limits.EndUserKeyID(id, digest)))
		if status, err := endUserChat(t.Context(), f.h, secret, routeSlug, step.user); status != 200 || err != nil {
			t.Fatalf("identified tokens: %d %v", status, err)
		}
		glEventually(t, "observed end-user tokens", func() bool { return limHash(t, f.valkey, key)["tpm"] == step.tokens })
	}
	detail := f.h.want(f.owner, "GET", "/api/v1/api-keys/"+id, nil, nil, 200)
	f.h.want(f.owner, "PATCH", "/api/v1/api-keys/"+id, map[string]any{"end_user_policy": map[string]any{"defaults": map[string]any{"tokens_per_minute": 1}}}, etagHeader(detail), 200)
	f.h.refresh()
	before := f.vendor.chats.Load()
	if status, err := endUserChat(t.Context(), f.h, secret, routeSlug, "oversized"); status != 400 || err != nil {
		t.Fatalf("oversized end-user request: %d %v", status, err)
	}
	if before != f.vendor.chats.Load() {
		t.Fatal("token refusal dispatched")
	}

	detail = f.h.want(f.owner, "GET", "/api/v1/api-keys/"+id, nil, nil, 200)
	f.h.want(f.owner, "PATCH", "/api/v1/api-keys/"+id, map[string]any{"end_user_policy": map[string]any{"defaults": map[string]any{"tokens_per_minute": 50}}}, etagHeader(detail), 200)
	f.h.refresh()
	replica := newAccessHarnessOn(t, f.h.Pool, f.h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), f.namespace), nil, slog.New(slog.DiscardHandler))
	replica.refresh()
	limSettleInMinute(t, f.valkey, 10*time.Second)
	f.vendor.delay.Store(int64(20 * time.Millisecond))
	before = f.vendor.chats.Load()
	var wg sync.WaitGroup
	results := make(chan int, 20)
	for i := range 20 {
		h := f.h
		if i%2 != 0 {
			h = replica
		}
		wg.Go(func() {
			status, err := endUserChatMode(t.Context(), h, secret, routeSlug, "concurrent", true)
			if err != nil {
				t.Error(err)
			}
			results <- status
		})
	}
	wg.Wait()
	close(results)
	admitted := 0
	for status := range results {
		switch status {
		case 200:
			admitted++
		case 429:
		default:
			t.Fatalf("concurrent token admission: %d", status)
		}
	}
	if admitted == 0 || admitted > 5 || f.vendor.chats.Load()-before != int64(admitted) {
		t.Fatalf("token limit admitted %d ten-token requests; dispatched %d", admitted, f.vendor.chats.Load()-before)
	}
	digest := f.h.want(f.owner, "POST", "/api/v1/api-keys/"+id+"/end-user", map[string]any{"identifier": "concurrent"}, nil, 200)["end_user_digest"].(string)
	key, _ := limRateKeys(f.namespace, limits.EndUserLookup(limits.EndUserKeyID(id, digest)))
	glEventually(t, "concurrent tokens reconciled across replicas", func() bool {
		return limHash(t, f.valkey, key)["tpm"] == fmt.Sprint(admitted*10)
	})
}

func TestEndUserBudgetInitializesAndSettlesThroughAccounting(t *testing.T) {
	f := glSeedIn(t, "end-user-budget", glPrice{input: "1000000", output: "1000000"})
	id, secret := f.key("budget", map[string]any{"end_user_source": "native", "end_user_policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": "1"}}})
	await := endUserAccounting(t, f.h, f.limiter)
	before := f.vendor.chats.Load()
	if status, err := endUserChat(t.Context(), f.h, secret, routeSlug, "cost-user"); status != 503 || err != nil {
		t.Fatalf("unknown balance: %d %v", status, err)
	}
	await()
	if status, err := endUserChat(t.Context(), f.h, secret, routeSlug, "cost-user"); status != 429 || err != nil {
		t.Fatalf("in-flight estimate exceeds end-user budget: %d %v", status, err)
	}
	await()
	if f.vendor.chats.Load() != before {
		t.Fatal("unknown or insufficient balance dispatched")
	}
	detail := f.h.want(f.owner, "GET", "/api/v1/api-keys/"+id, nil, nil, 200)
	f.h.want(f.owner, "PATCH", "/api/v1/api-keys/"+id, map[string]any{"end_user_policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": "1000"}}}, etagHeader(detail), 200)
	f.h.refresh()
	if status, err := endUserChat(t.Context(), f.h, secret, routeSlug, "cost-user"); status != 200 || err != nil {
		t.Fatalf("known balance: %d %v", status, err)
	}
	await()
	var accrued string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT w.accrued::text FROM olp.end_user_cost_windows w JOIN olp.end_user_accounts a ON a.id=w.account_id WHERE a.api_key_id=$1 AND w.window_kind='day'`, id).Scan(&accrued); err != nil {
		t.Fatal(err)
	}
	if accrued != "10.000000000000" {
		t.Fatalf("settled cost = %s, want fixture's 10 tokens", accrued)
	}
	// Reconciliation and duplicate delivery must preserve the same balance.
	conn, err := f.h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Now()); err != nil {
		t.Fatal(fmt.Errorf("reconcile: %w", err))
	}
}

func TestProjectEndUserPoliciesEnforceBothBoundaries(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "End users"}, idem("project"), 201)
	projectID := project["id"].(string)
	path := "/api/v1/projects/" + projectID + "/end-user-policy"
	initial := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{}, etagHeader(initial), 422)
	h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"defaults": map[string]any{"requests_per_minute": 3}}}, etagHeader(initial), 200)
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Scoped provider", fixture.URL, &projectID, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"project_id": projectID, "slug": "end-user-project", "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"},
		"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem("draft"), 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, idem("activate")), 200)
	secrets := make([]string, 2)
	for i := range secrets {
		key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": fmt.Sprintf("key-%d", i), "project_id": projectID, "scopes": []string{"inference"}, "allowed_routes": []string{"end-user-project"}, "end_user_source": "native", "end_user_policy": map[string]any{"defaults": map[string]any{"requests_per_minute": 2}}}, idem(fmt.Sprintf("key-%d", i)), 201)
		secrets[i] = key["secret"].(string)
	}
	c := limClient(t)
	namespace := limNamespace(t, c, "project-end-users")
	h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, c, namespace), nil, slog.New(slog.DiscardHandler))
	h.refresh()
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), namespace), nil, slog.New(slog.DiscardHandler))
	replica.refresh()
	limSettleInMinute(t, c, 10*time.Second)
	for _, step := range []struct {
		key, status int
		user        string
	}{
		{0, 200, "shared"}, {0, 200, "shared"}, {0, 429, "shared"},
		// The key refusal refunds its project reservation, leaving one request.
		{1, 200, "shared"}, {1, 429, "shared"}, {1, 200, "other"},
	} {
		if status, err := endUserChat(t.Context(), replica, secrets[step.key], "end-user-project", step.user); status != step.status || err != nil {
			t.Fatalf("step %+v: %d %v", step, status, err)
		}
	}
	t.Run("cost is shared across keys", func(t *testing.T) {
		h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{
			"effective_at": time.Now().UTC().Format(time.RFC3339Nano),
			"prices":       []any{map[string]any{"provider_kind": "azure_openai", "provider_id": provider["id"], "model": vendorModel, "operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}},
		}, idem("project-price"), 201)
		setBudget := func(amount string) {
			detail := h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": amount}}}, etagHeader(detail), 200)
			h.refresh()
			replica.refresh()
		}
		setBudget("1000")
		glEventually(t, "project price publication", func() bool {
			h.refresh()
			replica.refresh()
			inputs := replica.Runtime.RoutingInputs()
			return inputs != nil && len(inputs.Prices) > 0
		})
		limiter := limLimiter(t, c, namespace)
		await := endUserAccounting(t, h, limiter)
		replica.Gateway.Sink = h.Gateway.Sink
		for _, step := range []struct{ key, status int }{{0, 503}, {1, 200}} {
			if status, err := endUserChat(t.Context(), replica, secrets[step.key], "end-user-project", "priced-user"); status != step.status || err != nil {
				t.Fatalf("shared budget admission: %d %v", status, err)
			}
			await()
		}
		var accrued, digest string
		if err := h.Pool.QueryRow(t.Context(), `SELECT w.accrued::text,a.end_user_digest FROM olp.end_user_cost_windows w JOIN olp.end_user_accounts a ON a.id=w.account_id WHERE a.project_id=$1 AND w.window_kind='day'`, projectID).Scan(&accrued, &digest); err != nil {
			t.Fatal(err)
		}
		if accrued != "2.000000000000" {
			t.Fatalf("project cost=%s, want two billed fixture tokens", accrued)
		}
		setBudget(accrued)
		if status, err := endUserChat(t.Context(), h, secrets[0], "end-user-project", "priced-user"); status != 429 || err != nil {
			t.Fatalf("spend on another key bypassed project budget: %d %v", status, err)
		}
		await()
		conn, err := h.Pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, snapshot := range snapshots {
			if snapshot.CostOwnerID == limits.EndUserProjectID(projectID, digest) {
				found = true
				if snapshot.DailyAccrued != accrued {
					t.Fatalf("project reconciliation: %+v", snapshot)
				}
			}
		}
		if !found {
			t.Fatal("project identity missing from reconciliation")
		}
	})

}

func TestProjectEndUserPolicyPromotionPreservesLocalDigestControls(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Portable"}, idem("project"), 201)
	path := "/api/v1/projects/" + project["id"].(string) + "/end-user-policy"
	initial := h.want(owner, "GET", path, nil, nil, 200)
	digest := strings.Repeat("a", 64)
	h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"defaults": map[string]any{"requests_per_minute": 3}, "overrides": map[string]any{digest: map[string]any{"requests_per_minute": 1}}, "blocked": []string{digest}}}, etagHeader(initial), 200)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	encoded, _ := json.Marshal(document)
	if strings.Contains(string(encoded), digest) {
		t.Fatal("installation-local digest was exported")
	}
	entry := document["projects"].([]any)[0].(map[string]any)
	entry["end_user_defaults"].(map[string]any)["requests_per_minute"] = 11
	body := map[string]any{"document": document}
	configure := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Configure only", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("configure-only"), 201)["secret"].(string)
	h.machineWant(configure, "POST", "/api/v1/configuration/apply", body, idem("cannot-change-policy"), 403)
	unchanged := h.want(owner, "GET", path, nil, nil, 200)["policy"].(map[string]any)
	if unchanged["defaults"].(map[string]any)["requests_per_minute"] != float64(3) {
		t.Fatal("denied promotion changed policy")
	}
	planned := h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	found := false
	for _, action := range planned["actions"].([]any) {
		item := action.(map[string]any)
		found = found || item["kind"] == "project" && item["action"] == "replace"
	}
	if !found {
		t.Fatalf("plan did not identify project update: %v", planned)
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("apply"), 200)
	h.machineWant(configure, "POST", "/api/v1/configuration/apply", body, idem("unchanged-policy"), 200)
	policy := h.want(owner, "GET", path, nil, nil, 200)["policy"].(map[string]any)
	if policy["defaults"].(map[string]any)["requests_per_minute"] != float64(11) || len(policy["overrides"].(map[string]any)) != 1 || policy["blocked"].([]any)[0] != digest {
		t.Fatalf("promotion lost policy: %v", policy)
	}
}

func endUserAccounting(t *testing.T, h *accessHarness, limiter *limits.Limiter) func() {
	t.Helper()
	finished := make(chan error, 16)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		persisted, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		if err == nil {
			for _, snapshot := range persisted.CostSnapshots {
				if _, _, err = limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
					break
				}
			}
		}
		finished <- err
		return err
	}}
	return func() {
		t.Helper()
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("accounting did not complete")
		}
	}
}

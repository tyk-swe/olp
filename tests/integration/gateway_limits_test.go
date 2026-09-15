//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

// glFixture is a provisioned installation whose gateway enforces its budgets
// through a real limiter in a namespace of its own.
type glFixture struct {
	h        *accessHarness
	owner    *browser
	vendor   *vendor
	limiter  *limits.Limiter
	provider string
	path     string
}

// glSeed provisions one provider, one route, and the admission that enforces
// the budgets the scenario sets.
func glSeed(t *testing.T, limiter *limits.Limiter, policy limits.OutagePolicy, targetTimeout ...int) *glFixture {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "Fixture vendor", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	f := &glFixture{h: h, owner: owner, vendor: up, limiter: limiter, provider: created["id"].(string)}
	f.path = "/api/v3/providers/" + f.provider
	if probe := h.want(owner, "POST", f.path+"/probe", nil, etagHeader(created), 200); probe["succeeded"] != true {
		t.Fatalf("probe failed: %v", probe)
	}
	models := h.want(owner, "GET", f.path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail := h.want(owner, "GET", f.path, nil, nil, 200)
	if certified := h.want(owner, "POST", f.path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200); certified["status"] != "certified" {
		t.Fatalf("certification failed: %v", certified)
	}
	f.activate("activate-initial")
	timeout := 8000
	if len(targetTimeout) > 0 {
		timeout = targetTimeout[0]
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 10000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_id": f.provider, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": timeout}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v3/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	h.Gateway.Admission = gateway.NewAdmission(limiter, func() limits.OutagePolicy { return policy }, slog.New(slog.DiscardHandler))
	h.refresh()
	return f
}

// activate publishes the provider's pending configuration.
func (f *glFixture) activate(idempotency string) {
	f.h.t.Helper()
	detail := f.h.want(f.owner, "GET", f.path, nil, nil, 200)
	f.h.want(f.owner, "POST", f.path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": idempotency}), 200)
}

// key mints an inference key carrying one budget policy and returns its
// identifier and secret.
func (f *glFixture) key(name string, policy map[string]any) (string, string) {
	f.h.t.Helper()
	body := map[string]any{"name": name, "scopes": []string{"inference"}, "allowed_routes": []string{routeSlug}}
	for field, value := range policy {
		body[field] = value
	}
	created := f.h.want(f.owner, "POST", "/api/v3/api-keys", body, map[string]string{"Idempotency-Key": "key-" + name}, 201)
	f.h.refresh()
	return created["id"].(string), created["secret"].(string)
}

// chat is one inference request and the status, code, and retry hint it came
// back with.
func (f *glFixture) chat(key string) (int, string, http.Header) {
	f.h.t.Helper()
	status, body, header := f.h.gateway("POST", "/v1/chat/completions", key, map[string]any{
		"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if status == http.StatusOK {
		return status, "", header
	}
	return status, f.h.gatewayCode(status, body), header
}

// glRetryAfter reads the retry hint a rejection advertised.
func glRetryAfter(t *testing.T, header http.Header) int {
	t.Helper()
	raw := header.Get("Retry-After")
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After %q is not a whole number of seconds: %v", raw, err)
	}
	return seconds
}

// glStream runs a streaming request to completion and reports the status it
// received. Errors are returned rather than failed on: the caller runs it in
// its own goroutine, and a cancelled client is a scenario, not a fault.
func glStream(ctx context.Context, h *accessHarness, key string) (int, error) {
	body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.HTTP.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, err
}

// glEventually waits for a condition that a settlement outside the request
// path decides, such as a concurrency slot coming back.
func glEventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// glInFlight waits until the upstream has taken the request a caller started.
func glInFlight(t *testing.T, up *vendor, before int64) {
	t.Helper()
	glEventually(t, "the upstream to receive the request", func() bool { return up.chats.Load() > before })
}

// TestGatewayEnforcesKeyBudgets checks the limits an API key carries: the
// request and token windows, the concurrency slot a request holds and gives
// back, and a cost budget that can only be spent against a known balance.
func TestGatewayEnforcesKeyBudgets(t *testing.T) {
	c := limClient(t)
	f := glSeed(t, limLimiter(t, c, limNamespace(t, c, "gateway-key")), limits.FailClosed)

	t.Run("requests per minute", func(t *testing.T) {
		_, secret := f.key("rpm", map[string]any{"requests_per_minute": 1})
		if status, code, _ := f.chat(secret); status != http.StatusOK {
			t.Fatalf("first request: %d %s", status, code)
		}
		served := f.vendor.chats.Load()
		status, code, header := f.chat(secret)
		if status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
			t.Fatalf("second request: %d %s", status, code)
		}
		if retry := glRetryAfter(t, header); retry < 1 || retry > 60 {
			t.Fatalf("Retry-After %d is outside the window it refers to", retry)
		}
		if f.vendor.chats.Load() != served {
			t.Fatal("a rejected request reached the upstream")
		}
	})

	t.Run("a request larger than the token window", func(t *testing.T) {
		_, secret := f.key("tpm", map[string]any{"tokens_per_minute": 16})
		served := f.vendor.chats.Load()
		status, code, header := f.chat(secret)
		if status != http.StatusBadRequest || code != "request_exceeds_token_limit" {
			t.Fatalf("oversized request: %d %s", status, code)
		}
		if retry := header.Get("Retry-After"); retry != "" {
			t.Fatalf("Retry-After %q offered for a request no wait can fix", retry)
		}
		if f.vendor.chats.Load() != served {
			t.Fatal("a rejected request reached the upstream")
		}
	})

	t.Run("concurrency is held for the request and returned after it", func(t *testing.T) {
		_, secret := f.key("concurrency", map[string]any{"max_concurrency": 1})
		f.vendor.delay.Store(int64(200 * time.Millisecond))
		defer f.vendor.delay.Store(0)
		served := f.vendor.chats.Load()
		streamed := make(chan int, 1)
		go func() {
			status, _ := glStream(t.Context(), f.h, secret)
			streamed <- status
		}()
		glInFlight(t, f.vendor, served)
		status, code, header := f.chat(secret)
		if status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
			t.Fatalf("second request while one was in flight: %d %s", status, code)
		}
		if retry := glRetryAfter(t, header); retry < 1 || retry > 5 {
			t.Fatalf("Retry-After %d for a slot that frees as soon as the request ends", retry)
		}
		if status := <-streamed; status != http.StatusOK {
			t.Fatalf("streamed request: %d", status)
		}
		glEventually(t, "the concurrency slot to come back", func() bool {
			status, _, _ := f.chat(secret)
			return status == http.StatusOK
		})
	})

	t.Run("a client that hangs up returns its slot", func(t *testing.T) {
		_, secret := f.key("cancelled", map[string]any{"max_concurrency": 1})
		f.vendor.delay.Store(int64(200 * time.Millisecond))
		defer f.vendor.delay.Store(0)
		served := f.vendor.chats.Load()
		ctx, cancel := context.WithCancel(t.Context())
		abandoned := make(chan struct{})
		go func() {
			defer close(abandoned)
			glStream(ctx, f.h, secret)
		}()
		glInFlight(t, f.vendor, served)
		cancel()
		<-abandoned
		glEventually(t, "the abandoned request to return its slot", func() bool {
			status, _, _ := f.chat(secret)
			return status == http.StatusOK
		})
	})

	t.Run("a cost budget is only spent against a known balance", func(t *testing.T) {
		id, secret := f.key("budget", map[string]any{"daily_cost_limit": "5.00"})
		served := f.vendor.chats.Load()
		if status, code, _ := f.chat(secret); status != http.StatusServiceUnavailable || code != "distributed_limits_unavailable" {
			t.Fatalf("unreconciled budget: %d %s", status, code)
		}
		if f.vendor.chats.Load() != served {
			t.Fatal("a request the budget could not decide reached the upstream")
		}
		windows := limits.BudgetWindows(time.Now().UTC())
		snapshot := limits.CostSnapshot{
			APIKeyID: id, DailyWindowID: windows.DailyID, DailyAccrued: "0.00",
			MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0.00",
		}
		if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatalf("apply empty balance: %v", err)
		}
		if status, code, _ := f.chat(secret); status != http.StatusOK {
			t.Fatalf("request within budget: %d %s", status, code)
		}
		snapshot.DailyAccrued = "5.00"
		if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatalf("apply exhausted balance: %v", err)
		}
		served = f.vendor.chats.Load()
		status, body, header := f.h.gateway("POST", "/v1/chat/completions", secret, map[string]any{
			"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		code := f.h.gatewayCode(status, body)
		if status != http.StatusTooManyRequests || code != "budget_exhausted" {
			t.Fatalf("exhausted budget: %d %s", status, code)
		}
		// The message is part of the contract, not decoration: it is the only
		// place the API tells an operator why a request was refused while the
		// spend they can see still reads below the limit.
		const exhausted = "The API key cost budget was exhausted. Unpriced attempts accrue 0."
		if message, _ := body["error"].(map[string]any)["message"].(string); message != exhausted {
			t.Fatalf("exhausted budget message = %q, want %q", message, exhausted)
		}
		if retry := glRetryAfter(t, header); retry < 1 {
			t.Fatalf("Retry-After %d for a budget that resets on a window boundary", retry)
		}
		if f.vendor.chats.Load() != served {
			t.Fatal("a request over budget reached the upstream")
		}
	})
}

// TestGatewayEnforcesProviderQuotas checks the quota a credential slot carries:
// it is reserved before the provider is called, and an attempt it rejects
// never reaches the upstream.
func TestGatewayEnforcesProviderQuotas(t *testing.T) {
	c := limClient(t)
	f := glSeed(t, limLimiter(t, c, limNamespace(t, c, "gateway-slot")), limits.FailClosed)
	listPath := f.path + "/credential-slots"
	slots := f.h.want(f.owner, "GET", listPath, nil, nil, 200)
	slotID := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	f.h.want(f.owner, "PUT", listPath+"/"+slotID, map[string]any{
		"slot": map[string]any{"name": "default", "enabled": true, "requests_per_minute": 1},
	}, withMatch(slots, map[string]string{"Idempotency-Key": "slot-quota"}), 200)
	// A slot edit is a draft until the provider is published again.
	f.activate("activate-slot-quota")
	f.h.refresh()

	_, secret := f.key("slot-quota", nil)
	if status, code, _ := f.chat(secret); status != http.StatusOK {
		t.Fatalf("first request: %d %s", status, code)
	}
	served := f.vendor.chats.Load()
	status, code, header := f.chat(secret)
	if status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
		t.Fatalf("second request: %d %s", status, code)
	}
	if retry := glRetryAfter(t, header); retry < 1 || retry > 60 {
		t.Fatalf("Retry-After %d is outside the window it refers to", retry)
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("an attempt the slot quota rejected reached the upstream")
	}
}

// TestGatewayRefundsRequestsThatReachedNoProvider checks that a request no
// upstream ever received gives its reservation back. A route whose providers
// are all unreachable costs the upstreams nothing, so it must not spend the
// caller's request window either.
func TestGatewayRefundsRequestsThatReachedNoProvider(t *testing.T) {
	c := limClient(t)
	f := glSeed(t, limLimiter(t, c, limNamespace(t, c, "gateway-refund")), limits.FailClosed)
	listPath := f.path + "/credential-slots"
	slots := f.h.want(f.owner, "GET", listPath, nil, nil, 200)
	slotID := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	f.h.want(f.owner, "PUT", listPath+"/"+slotID, map[string]any{
		"slot": map[string]any{"name": "default", "enabled": true, "requests_per_minute": 1},
	}, withMatch(slots, map[string]string{"Idempotency-Key": "refund-slot"}), 200)
	f.activate("activate-refund-slot")
	_, secret := f.key("refund", map[string]any{"requests_per_minute": 1})
	// The upstream is gone: every attempt now fails before a byte is written.
	served := f.vendor.chats.Load()
	f.vendor.Close()
	for attempt := range 3 {
		status, code, _ := f.chat(secret)
		if status != http.StatusBadGateway || code != "upstream_unavailable" {
			t.Fatalf("request %d: %d %s, want the unreachable upstream", attempt+1, status, code)
		}
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("an attempt reached an upstream that was no longer listening")
	}
}

// glBrokenLimiter is a limiter whose connection is gone: every command fails
// the way a Valkey outage does, without the wait a firewalled host would add.
func glBrokenLimiter(t *testing.T) *limits.Limiter {
	t.Helper()
	cfg, err := coordination.Configuration(required(t, "OLP_TEST_VALKEY_URL"), "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := coordination.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := limits.New(c, "olp-go-test:limits:broken:"+rand.Text())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	return limiter
}

// TestGatewayLimitOutagePolicy checks what an unreachable limiter does to a
// request: an installation may choose to serve traffic it can no longer count,
// but never to spend a budget it can no longer read.
func TestGatewayLimitOutagePolicy(t *testing.T) {
	f := glSeed(t, glBrokenLimiter(t), limits.FailOpen)
	_, throttled := f.key("outage-rpm", map[string]any{"requests_per_minute": 1})
	_, budgeted := f.key("outage-budget", map[string]any{"daily_cost_limit": "5.00"})

	for range 2 {
		if status, code, _ := f.chat(throttled); status != http.StatusOK {
			t.Fatalf("rate limited key not served while failing open: %d %s", status, code)
		}
	}
	if total := f.h.Gateway.Admission.FailOpenTotal(); total != 2 {
		t.Fatalf("fail open total = %d, want 2", total)
	}
	served := f.vendor.chats.Load()
	if status, code, _ := f.chat(budgeted); status != http.StatusServiceUnavailable || code != "distributed_limits_unavailable" {
		t.Fatalf("cost budget served without a balance: %d %s", status, code)
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("a request whose budget could not be read reached the upstream")
	}
	if total := f.h.Gateway.Admission.FailOpenTotal(); total != 2 {
		t.Fatalf("fail open total = %d after a budgeted key failed closed, want 2", total)
	}

	f.h.Gateway.Admission = gateway.NewAdmission(glBrokenLimiter(t), func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	served = f.vendor.chats.Load()
	if status, code, _ := f.chat(throttled); status != http.StatusServiceUnavailable || code != "distributed_limits_unavailable" {
		t.Fatalf("rate limited key served while failing closed: %d %s", status, code)
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("a request that could not be counted reached the upstream")
	}
}

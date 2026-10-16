//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
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
	"github.com/tyk-swe/olp/internal/usage"
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
	// routeRevision is the revision ID the fixture's route was activated as, which
	// is what a response says served it.
	routeRevision string
	// valkey and namespace are where the fixture's limiter keeps its counters,
	// for a test that reads them directly. They are set by glSeedIn.
	valkey    *coordination.Client
	namespace string
}

// glSeed provisions one provider, one route, and the admission that enforces
// the budgets the scenario sets.
func glSeed(t *testing.T, limiter *limits.Limiter, policy limits.OutagePolicy, targetTimeout ...int) *glFixture {
	t.Helper()
	return glSeedPriced(t, limiter, policy, glPrice{}, targetTimeout...)
}

// glSeedIn is glSeedPriced over a limiter in a namespace of its own, which the
// fixture remembers so that a test can read the counters the limiter keeps.
func glSeedIn(t *testing.T, label string, price glPrice) *glFixture {
	t.Helper()
	return glSeedInFidelity(t, label, price, "transformed")
}

// glSeedInFidelity is glSeedIn with the fidelity mode of the route named.
func glSeedInFidelity(t *testing.T, label string, price glPrice, fidelity string) *glFixture {
	t.Helper()
	c, namespace, limiter := limSetup(t, label)
	f := glSeedFidelity(t, limiter, limits.FailClosed, price, fidelity)
	f.valkey, f.namespace = c, namespace
	return f
}

// glPrice is what the fixture model costs per million tokens, as decimal text.
// The zero value publishes no price list, so nothing a request does is priced.
type glPrice struct{ input, output string }

// glSeedPriced is glSeed with a price list for the fixture model, which is what
// lets admission reserve a request's cost before it is dispatched.
func glSeedPriced(t *testing.T, limiter *limits.Limiter, policy limits.OutagePolicy, price glPrice, targetTimeout ...int) *glFixture {
	t.Helper()
	return glSeedFidelity(t, limiter, policy, price, "transformed", targetTimeout...)
}

// glSeedFidelity is glSeedPriced with the fidelity mode of the route named: a
// strict route sends a target exactly what the caller sent, which is the mode a
// route has unless it says otherwise.
func glSeedFidelity(t *testing.T, limiter *limits.Limiter, policy limits.OutagePolicy, price glPrice, fidelity string, targetTimeout ...int) *glFixture {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	configuration := map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"}
	if fidelity == "strict" {
		// A strict route sends the target exactly what the caller sent, so the
		// provider has to declare a versioned profile that says what it accepts.
		configuration["profile_id"], configuration["profile_revision"] = "compatible-chat", "1"
	}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Fixture vendor", "model": vendorModel, "credential": vendorSecret,
		"configuration": configuration,
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	f := &glFixture{h: h, owner: owner, vendor: up, limiter: limiter, provider: created["id"].(string)}
	f.path = "/api/v1/providers/" + f.provider
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
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": fidelity},
		"targets": []any{map[string]any{"provider_id": f.provider, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": timeout}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	activated := h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	f.routeRevision, _ = activated["revision_id"].(string)
	h.Gateway.Admission = gateway.NewAdmission(limiter, func() limits.OutagePolicy { return policy }, slog.New(slog.DiscardHandler))
	if price != (glPrice{}) {
		h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{
			"effective_at": time.Now().UTC().Format(time.RFC3339Nano),
			"prices": []any{map[string]any{
				"provider_kind": "openai_compatible", "provider_id": f.provider, "model": vendorModel,
				"operation": "generation", "currency": "USD",
				"input_per_million": price.input, "output_per_million": price.output,
			}},
		}, map[string]string{"Idempotency-Key": "pricing"}, 201)
	}
	h.refresh()
	if price != (glPrice{}) {
		// The gateway reads prices from routing inputs that refresh at most every
		// few seconds, so a list published after the first load would not be seen
		// yet. It is published before it here; this only proves it was.
		eventually(t, "the price list to reach the gateway", 15*time.Second, func() bool {
			h.refresh()
			inputs := h.Runtime.RoutingInputs()
			return inputs != nil && len(inputs.Prices) > 0
		})
	}
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
	created := f.h.want(f.owner, "POST", "/api/v1/api-keys", body, map[string]string{"Idempotency-Key": "key-" + name}, 201)
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

// chatWith is chat with more fields in the request, such as a reply bound.
func (f *glFixture) chatWith(key string, fields map[string]any) (int, string, http.Header) {
	f.h.t.Helper()
	body := map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for name, value := range fields {
		body[name] = value
	}
	// Prices come from routing inputs that expire a minute after they were read,
	// so a test that runs for long refreshes them as the serving loop does.
	f.h.refresh()
	status, reply, header := f.h.gateway("POST", "/v1/chat/completions", key, body)
	if status == http.StatusOK {
		return status, "", header
	}
	return status, f.h.gatewayCode(status, reply), header
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

// glStreamWith is glStream with more fields in the request.
func glStreamWith(ctx context.Context, h *accessHarness, key string, fields map[string]any) (int, error) {
	body := map[string]any{"model": routeSlug, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for name, value := range fields {
		body[name] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.HTTP.URL+"/v1/chat/completions", bytes.NewReader(payload))
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

// glAccounting runs the fixture's usage events through the pipeline a process
// runs: the gateway emits them, the writer appends them to the stream, and a
// consumer prices and stores them and applies the spend they recorded to the
// same limiter that admits requests. It is what turns a settled reservation into
// accrued spend.
func glAccounting(t *testing.T, f *glFixture) {
	t.Helper()
	valkey, _, stream := acctKeyspace(t)
	emitter := usage.NewEmitter(64)
	f.h.Gateway.Sink = &gateway.AccountingSink{Emitter: emitter, Next: f.h.Gateway.Sink}
	ctx, cancel := context.WithCancel(t.Context())
	written := make(chan struct{})
	go func() {
		defer close(written)
		emitter.RunWriter(ctx, valkey, stream, slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() {
		emitter.Close()
		<-written
		cancel()
	})
	acctConsumer(t, f.h.Pool, acctValkey(t), stream, "gateway-accounting", f.limiter, 30*time.Second)
}

// glInFlight waits until the upstream has taken the request a caller started.
func glInFlight(t *testing.T, up *vendor, before int64) {
	t.Helper()
	eventually(t, "the upstream to receive the request", 15*time.Second, func() bool { return up.chats.Load() > before })
}

// The message is part of the contract, not decoration: it is the only
// place the API tells an operator why a request was refused while the
// spend they can see still reads below the limit.

// A slot edit is a draft until the provider is published again.

// The upstream is gone: every attempt now fails before a byte is written.

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
	limiter, err := limits.New(c, "olp-test:limits:broken:"+rand.Text())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	return limiter
}

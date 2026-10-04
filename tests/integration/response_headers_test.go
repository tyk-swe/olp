//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

// rhAnthropic is an Anthropic Messages upstream whose stream is paced by delay,
// so that a request stays in flight long enough for a test to read the window it
// holds. Every answer reports 4 input and 6 output tokens, as the OpenAI-compatible
// fixture vendor does.
type rhAnthropic struct {
	*httptest.Server
	delay atomic.Int64
	calls atomic.Int64
}

func newRHAnthropic(t *testing.T) *rhAnthropic {
	t.Helper()
	up := &rhAnthropic{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != vendorSecret {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel, "display_name": "Fixture"}}})
			return
		}
		up.calls.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		message := map[string]any{
			"id": "msg-fixture", "type": "message", "role": "assistant", "model": vendorModel,
			"content":     []any{map[string]string{"type": "text", "text": vendorAnswer}},
			"stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 4, "output_tokens": 6},
		}
		if stream, _ := body["stream"].(bool); !stream {
			writeJSON(w, message)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(name string, value any) {
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			w.(http.Flusher).Flush()
			select {
			case <-time.After(time.Duration(up.delay.Load())):
			case <-r.Context().Done():
			}
		}
		message["content"], message["stop_reason"] = []any{}, nil
		message["usage"] = map[string]int{"input_tokens": 4, "output_tokens": 0}
		emit("message_start", map[string]any{"type": "message_start", "message": message})
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		for _, word := range strings.SplitAfter(vendorAnswer, " ") {
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": word}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 6}})
		emit("message_stop", map[string]string{"type": "message_stop"})
	}))
	t.Cleanup(up.Close)
	return up
}

// The routes the fixture serves beside the glFixture's own: one that fails over
// from it to a second OpenAI-compatible upstream, and one that reaches an
// Anthropic upstream.
const (
	rhFailoverSlug  = "failover-chat"
	rhAnthropicSlug = "anthropic-chat"
)

// rhFixture is a glFixture that also serves those routes, each with the revision
// it was activated as, and whose three providers are priced alike.
type rhFixture struct {
	*glFixture
	second            *vendor
	anthropic         *rhAnthropic
	failoverRevision  string
	anthropicRevision string
	// vendorID and anthropicVendor are the vendors the OpenAI-compatible and the
	// Anthropic providers are configured with.
	vendorID        string
	anthropicVendor string
}

// slug is the route a surface's requests go to.
func (f *rhFixture) slug(surface string) string {
	if surface == "anthropic" {
		return rhAnthropicSlug
	}
	return routeSlug
}

// revision is the revision a surface's route was activated as.
func (f *rhFixture) revision(surface string) string {
	if surface == "anthropic" {
		return f.anthropicRevision
	}
	return f.routeRevision
}

// rhProvider creates, certifies and activates a provider on an upstream and
// returns its identifier and the vendor it is configured with.
func rhProvider(t *testing.T, f *glFixture, name, kind, endpoint string) (id, vendorID string) {
	t.Helper()
	h, owner := f.h, f.owner
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": name, "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": kind, "auth_mode": "api_key", "endpoint": endpoint},
	}, map[string]string{"Idempotency-Key": "provider-" + name}, 201)
	id = created["id"].(string)
	path := "/api/v1/providers/" + id
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(created), 200); probe["succeeded"] != true {
		t.Fatalf("%s: probe failed: %v", name, probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200); certified["status"] != "certified" {
		t.Fatalf("%s: certification failed: %v", name, certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate-" + name}), 200)
	vendorID, _ = h.want(owner, "GET", path, nil, nil, 200)["vendor_id"].(string)
	if vendorID == "" {
		t.Fatalf("%s has no vendor, which the metadata is to name", name)
	}
	return id, vendorID
}

// rhRoute publishes a route over targets, in priority order, and returns the
// revision it was activated as.
func rhRoute(t *testing.T, f *glFixture, slug string, targets ...string) string {
	t.Helper()
	h, owner := f.h, f.owner
	var list []any
	for priority, provider := range targets {
		list = append(list, map[string]any{"provider_id": provider, "provider_model": vendorModel, "priority": priority, "weight": 1, "timeout_ms": 8000})
	}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "overall_timeout_ms": 10000, "max_attempts": len(targets), "fidelity": map[string]any{"mode": "transformed"}, "targets": list,
	}, map[string]string{"Idempotency-Key": "draft-" + slug}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	activated := h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "activate-" + slug}), 200)
	return activated["revision_id"].(string)
}

// rhSeed provisions the fixture's providers and routes against a real limiter,
// and when price is not zero publishes it for all three providers.
func rhSeed(t *testing.T, label string, price glPrice) *rhFixture {
	t.Helper()
	g := glSeedIn(t, label, glPrice{})
	f := &rhFixture{glFixture: g, second: newVendor(t), anthropic: newRHAnthropic(t)}
	f.vendorID, _ = g.h.want(g.owner, "GET", g.path, nil, nil, 200)["vendor_id"].(string)
	if f.vendorID == "" {
		t.Fatal("the provider has no vendor, which the metadata is to name")
	}
	second, _ := rhProvider(t, g, "Fallback vendor", "openai_compatible", f.second.URL+"/v1")
	anthropic, anthropicVendor := rhProvider(t, g, "Anthropic vendor", "anthropic", f.anthropic.URL+"/v1")
	f.anthropicVendor = anthropicVendor
	f.failoverRevision = rhRoute(t, g, rhFailoverSlug, g.provider, second)
	f.anthropicRevision = rhRoute(t, g, rhAnthropicSlug, anthropic)
	if price != (glPrice{}) {
		var prices []any
		for provider, kind := range map[string]string{g.provider: "openai_compatible", second: "openai_compatible", anthropic: "anthropic"} {
			prices = append(prices, map[string]any{
				"provider_kind": kind, "provider_id": provider, "model": vendorModel, "operation": "generation", "currency": "USD",
				"input_per_million": price.input, "output_per_million": price.output,
			})
		}
		g.h.want(g.owner, "POST", "/api/v1/pricing/revisions", map[string]any{
			"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": prices,
		}, map[string]string{"Idempotency-Key": "pricing-" + label}, 201)
	}
	g.h.refresh()
	if price != (glPrice{}) {
		glEventually(t, "the price list to reach the gateway", func() bool {
			g.h.refresh()
			inputs := g.h.Runtime.RoutingInputs()
			return inputs != nil && len(inputs.Prices) >= 3
		})
	}
	return f
}

// mint creates a key that may use every route of the fixture and returns its
// identifier, its secret, and the Valkey key its rate window lives in: the lookup
// is the second segment of the secret.
func (f *rhFixture) mint(t *testing.T, name string, policy map[string]any) (id, secret, rateKey string) {
	t.Helper()
	body := map[string]any{"allowed_routes": []string{routeSlug, rhFailoverSlug, rhAnthropicSlug}}
	for field, value := range policy {
		body[field] = value
	}
	id, secret = f.glFixture.key(name, body)
	rateKey, _ = limRateKeys(f.namespace, strings.SplitN(secret, "_", 3)[1])
	return id, secret, rateKey
}

// key is mint without the identifier.
func (f *rhFixture) key(t *testing.T, name string, policy map[string]any) (secret, rateKey string) {
	t.Helper()
	_, secret, rateKey = f.mint(t, name, policy)
	return secret, rateKey
}

// budgetKey mints a key bound by a daily cost budget of 5.00 and reconciles its
// balance to accrued, because a budget is only spent against a known balance.
func (f *rhFixture) budgetKey(t *testing.T, name, accrued string, policy map[string]any) string {
	t.Helper()
	body := map[string]any{"daily_cost_limit": "5.00"}
	for field, value := range policy {
		body[field] = value
	}
	id, secret, _ := f.mint(t, name, body)
	windows := limits.BudgetWindows(time.Now().UTC())
	snapshot := limits.CostSnapshot{
		CostOwnerID: id, DailyWindowID: windows.DailyID, DailyAccrued: accrued,
		MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0.00",
	}
	if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
		t.Fatalf("reconcile the balance: %v", err)
	}
	return secret
}

// open sends one generation request for a route on a surface and returns the
// response as soon as its headers have arrived. A stream's headers are committed
// with its first frame, so the body is still being written when this returns:
// the caller reads what Valkey holds meanwhile, and drains the body with rhDrain.
func (f *rhFixture) open(t *testing.T, surface, slug, secret string, stream bool) *http.Response {
	t.Helper()
	// Prices expire a minute after the gateway read them.
	f.h.refresh()
	path := "/v1/chat/completions"
	if surface == "anthropic" {
		path = "/anthropic/v1/messages"
	}
	payload, err := json.Marshal(map[string]any{
		"model": slug, "max_tokens": 16, "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.h.HTTP.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if surface == "anthropic" {
		req.Header.Set("X-Api-Key", secret)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// pace makes the streams of both fixture upstreams last as long as delay between
// each frame.
func (f *rhFixture) pace(delay time.Duration) {
	f.vendor.delay.Store(int64(delay))
	f.anthropic.delay.Store(int64(delay))
}

// rhDrain reads a response to its end and closes it.
func rhDrain(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// rhWindow reads the counters of a key's window.
func rhWindow(t *testing.T, f *rhFixture, rateKey string) (requests, tokens int64) {
	t.Helper()
	hash := limHash(t, f.valkey, rateKey)
	return limField(t, hash, "rpm"), limField(t, hash, "tpm")
}

func rhInt(t *testing.T, header http.Header, name string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(header.Get(name), 10, 64)
	if err != nil {
		t.Fatalf("%s = %q, which is not a count: %v (headers %v)", name, header.Get(name), err, header)
	}
	return value
}

// rhRateHeaders names the headers of a surface that state an allowance: the
// request and token limits, what remains of each, and when each resets.
func rhRateHeaders(surface string) (limitRequests, remainingRequests, resetRequests, limitTokens, remainingTokens, resetTokens string) {
	if surface == "anthropic" {
		return "Anthropic-Ratelimit-Requests-Limit", "Anthropic-Ratelimit-Requests-Remaining", "Anthropic-Ratelimit-Requests-Reset",
			"Anthropic-Ratelimit-Tokens-Limit", "Anthropic-Ratelimit-Tokens-Remaining", "Anthropic-Ratelimit-Tokens-Reset"
	}
	return "X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Requests", "X-Ratelimit-Reset-Requests",
		"X-Ratelimit-Limit-Tokens", "X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Reset-Tokens"
}

// rhRateHeaderCount counts the rate-limit headers a response carries on any surface.
func rhRateHeaderCount(header http.Header) int {
	count := 0
	for name := range header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-ratelimit-") || strings.HasPrefix(lower, "anthropic-ratelimit-") {
			count++
		}
	}
	return count
}

// rhSameMinute fails a test whose requests did not all run in the minute it began
// in: a window that rolled over states counts that no longer match the counters a
// refusal left behind, which is a property of the clock and not of the headers.
func rhSameMinute(t *testing.T, f *rhFixture, start int64) {
	t.Helper()
	if now := limServerTimeMS(t, f.valkey); now/60_000 != start/60_000 {
		t.Fatalf("the test spanned a minute boundary, from %d to %d", start, now)
	}
}

// rhWantReset checks that a reset states the end of the minute Valkey was in
// while the request was made, readings of its clock bracketing the request. The
// OpenAI form is the time left, counted down to when the response was written,
// and the Anthropic form is the instant the minute ends.
func rhWantReset(t *testing.T, surface, name, value string, before, after int64) {
	t.Helper()
	if before/60_000 != after/60_000 {
		t.Fatalf("the request spanned a minute boundary: %d to %d", before, after)
	}
	if surface == "anthropic" {
		end := time.UnixMilli((before/60_000 + 1) * 60_000).UTC()
		if got, err := time.Parse(time.RFC3339, value); err != nil || !got.Equal(end) || value != end.Format(time.RFC3339) {
			t.Fatalf("%s = %q, want %s", name, value, end.Format(time.RFC3339))
		}
		return
	}
	left, err := time.ParseDuration(value)
	if err != nil || left.String() != value || left%time.Millisecond != 0 {
		t.Fatalf("%s = %q, which is not a whole number of milliseconds in a duration as OpenAI writes one: %v", name, value, err)
	}
	longest := time.Duration(60_000-before%60_000) * time.Millisecond
	shortest := time.Duration(60_000-after%60_000) * time.Millisecond
	if left > longest || left < shortest-150*time.Millisecond {
		t.Fatalf("%s = %s, want what is left of the minute, within [%s, %s]", name, left, shortest, longest)
	}
}

// TestResponseHeadersMatchTheKeysValkeyWindow reads a key's window while its
// stream is still open, when it holds exactly the reservation the request made,
// and holds the headers on the first frame to it, on both surfaces: the limits,
// the remaining counts, and the reset.
func TestResponseHeadersMatchTheKeysValkeyWindow(t *testing.T) {
	f := rhSeed(t, "response-headers-window", glPrice{})
	f.pace(200 * time.Millisecond)
	const requestLimit, tokenLimit = 20, 100_000
	for _, surface := range []string{"openai", "anthropic"} {
		t.Run(surface, func(t *testing.T) {
			secret, rateKey := f.key(t, "window-"+surface, map[string]any{"requests_per_minute": requestLimit, "tokens_per_minute": tokenLimit})
			limitRequests, remainingRequests, resetRequests, limitTokens, remainingTokens, resetTokens := rhRateHeaders(surface)
			start := limSettleInMinute(t, f.valkey, 30*time.Second)
			var reserved int64
			for n := int64(1); n <= 3; n++ {
				before := limServerTimeMS(t, f.valkey)
				resp := f.open(t, surface, f.slug(surface), secret, true)
				after := limServerTimeMS(t, f.valkey)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("request %d: status %d %s", n, resp.StatusCode, rhDrain(t, resp))
				}
				// The upstream is still answering: the window holds what earlier
				// requests settled at and what this one reserved, which is what
				// the headers must have stated.
				requests, tokens := rhWindow(t, f, rateKey)
				rhSameMinute(t, f, start)
				if requests != n {
					t.Fatalf("request %d: window counts %d requests", n, requests)
				}
				if got := rhInt(t, resp.Header, limitRequests); got != requestLimit {
					t.Fatalf("request %d: %s = %d", n, limitRequests, got)
				}
				if got := rhInt(t, resp.Header, remainingRequests); got != requestLimit-requests {
					t.Fatalf("request %d: %s = %d, want %d: the window holds %d", n, remainingRequests, got, requestLimit-requests, requests)
				}
				if got := rhInt(t, resp.Header, limitTokens); got != tokenLimit {
					t.Fatalf("request %d: %s = %d", n, limitTokens, got)
				}
				if got := rhInt(t, resp.Header, remainingTokens); got != tokenLimit-tokens {
					t.Fatalf("request %d: %s = %d, want %d: the window holds %d", n, remainingTokens, got, tokenLimit-tokens, tokens)
				}
				for _, name := range []string{resetRequests, resetTokens} {
					rhWantReset(t, surface, name, resp.Header.Get(name), before, after)
				}
				// Every request reserves the same estimate, on top of what the
				// earlier ones were settled at.
				if estimate := tokens - reserved*(n-1); n == 1 {
					reserved = estimate
				} else if estimate != reserved {
					t.Fatalf("request %d reserved %d tokens, the first reserved %d", n, estimate, reserved)
				}
				if rhRateHeaderCount(resp.Header) != 6 {
					t.Fatalf("request %d: headers %v", n, resp.Header)
				}
				rhDrain(t, resp)
				// Low provider usage cannot refund the conservative reservation.
				glEventually(t, "the reservation to be reconciled", func() bool {
					_, settled := rhWindow(t, f, rateKey)
					return settled == reserved*n
				})
			}
			// A unary response states the same window.
			resp := f.open(t, surface, f.slug(surface), secret, false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("unary: status %d %s", resp.StatusCode, rhDrain(t, resp))
			}
			rhDrain(t, resp)
			rhSameMinute(t, f, start)
			if got := rhInt(t, resp.Header, remainingRequests); got != requestLimit-4 {
				t.Fatalf("unary: %s = %d, want %d", remainingRequests, got, requestLimit-4)
			}
			if got, want := rhInt(t, resp.Header, remainingTokens), int64(tokenLimit)-4*reserved; got != want {
				t.Fatalf("unary: %s = %d, want %d: three settled requests and this one's reservation", remainingTokens, got, want)
			}
		})
	}
}

// TestResponseHeadersAreOnlyForTheDimensionsAKeyLimits proves the headers name
// the dimensions the key limits and say nothing of the rest, and that a key with
// nothing to count, or with only a lease to hold, gets none.
func TestResponseHeadersAreOnlyForTheDimensionsAKeyLimits(t *testing.T) {
	f := rhSeed(t, "response-headers-dimensions", glPrice{})
	for _, surface := range []string{"openai", "anthropic"} {
		limitRequests, _, _, limitTokens, _, _ := rhRateHeaders(surface)
		for name, tc := range map[string]struct {
			policy map[string]any
			budget bool
			want   int
			header string
		}{
			"requests":    {map[string]any{"requests_per_minute": 7}, false, 3, limitRequests},
			"tokens":      {map[string]any{"tokens_per_minute": 100_000}, false, 3, limitTokens},
			"concurrency": {map[string]any{"max_concurrency": 3}, false, 0, ""},
			"budget":      {nil, true, 0, ""},
			"unlimited":   {nil, false, 0, ""},
		} {
			t.Run(surface+"/"+name, func(t *testing.T) {
				var secret string
				if tc.budget {
					secret = f.budgetKey(t, surface+"-"+name, "0.00", nil)
				} else {
					secret, _ = f.key(t, surface+"-"+name, tc.policy)
				}
				for _, stream := range []bool{false, true} {
					resp := f.open(t, surface, f.slug(surface), secret, stream)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("stream=%v: status %d %s", stream, resp.StatusCode, rhDrain(t, resp))
					}
					rhDrain(t, resp)
					if got := rhRateHeaderCount(resp.Header); got != tc.want {
						t.Fatalf("stream=%v: %d rate-limit headers, want %d: %v", stream, got, tc.want, resp.Header)
					}
					if tc.header != "" && resp.Header.Get(tc.header) == "" {
						t.Fatalf("stream=%v: %s missing: %v", stream, tc.header, resp.Header)
					}
				}
			})
		}
	}
}

// TestRateLimitRejectionsCarryTheWindowAndTheRetryHint holds the headers of a 429
// to the window that refused it, on both surfaces and for each limit that can.
func TestRateLimitRejectionsCarryTheWindowAndTheRetryHint(t *testing.T) {
	f := rhSeed(t, "response-headers-rejections", glPrice{})
	for _, surface := range []string{"openai", "anthropic"} {
		t.Run(surface+"/requests", func(t *testing.T) {
			secret, rateKey := f.key(t, surface+"-rpm", map[string]any{"requests_per_minute": 1, "tokens_per_minute": 100_000})
			limitRequests, remainingRequests, resetRequests, limitTokens, remainingTokens, resetTokens := rhRateHeaders(surface)
			limSettleInMinute(t, f.valkey, 15*time.Second)
			resp := f.open(t, surface, f.slug(surface), secret, false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("first request: status %d %s", resp.StatusCode, rhDrain(t, resp))
			}
			rhDrain(t, resp)
			glEventually(t, "the first request to settle", func() bool { _, tokens := rhWindow(t, f, rateKey); return tokens == 10 })

			before := limServerTimeMS(t, f.valkey)
			resp = f.open(t, surface, f.slug(surface), secret, false)
			after := limServerTimeMS(t, f.valkey)
			body := rhDrain(t, resp)
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("second request: status %d %s", resp.StatusCode, body)
			}
			if retry := glRetryAfter(t, resp.Header); retry < 1 || retry > 60 {
				t.Fatalf("Retry-After = %d", retry)
			}
			requests, tokens := rhWindow(t, f, rateKey)
			// The refusal reserved nothing, so what it states is what the window holds.
			if requests != 1 || tokens != 10 {
				t.Fatalf("a refused request changed the window to %d requests and %d tokens", requests, tokens)
			}
			if got := rhInt(t, resp.Header, limitRequests); got != 1 {
				t.Fatalf("%s = %d", limitRequests, got)
			}
			if got := rhInt(t, resp.Header, remainingRequests); got != 0 {
				t.Fatalf("%s = %d", remainingRequests, got)
			}
			if got := rhInt(t, resp.Header, limitTokens); got != 100_000 {
				t.Fatalf("%s = %d", limitTokens, got)
			}
			if got := rhInt(t, resp.Header, remainingTokens); got != 100_000-tokens {
				t.Fatalf("%s = %d, want %d", remainingTokens, got, 100_000-tokens)
			}
			for _, name := range []string{resetRequests, resetTokens} {
				rhWantReset(t, surface, name, resp.Header.Get(name), before, after)
			}
		})

		t.Run(surface+"/concurrency", func(t *testing.T) {
			secret, rateKey := f.key(t, surface+"-concurrency", map[string]any{"requests_per_minute": 10, "tokens_per_minute": 100_000, "max_concurrency": 1})
			limitRequests, remainingRequests, resetRequests, _, remainingTokens, resetTokens := rhRateHeaders(surface)
			f.pace(300 * time.Millisecond)
			defer f.pace(0)
			start := limSettleInMinute(t, f.valkey, 15*time.Second)
			held := f.open(t, surface, f.slug(surface), secret, true)
			if held.StatusCode != http.StatusOK {
				t.Fatalf("held stream: status %d %s", held.StatusCode, rhDrain(t, held))
			}
			// The window while the slot is held: the first request's reservation,
			// which the refusal must report without counting itself.
			before := limServerTimeMS(t, f.valkey)
			resp := f.open(t, surface, f.slug(surface), secret, false)
			after := limServerTimeMS(t, f.valkey)
			body := rhDrain(t, resp)
			requests, tokens := rhWindow(t, f, rateKey)
			rhSameMinute(t, f, start)
			rhDrain(t, held)
			if resp.StatusCode != http.StatusTooManyRequests || !bytes.Contains(body, []byte("concurrency")) {
				t.Fatalf("second request: status %d %s", resp.StatusCode, body)
			}
			if retry := glRetryAfter(t, resp.Header); retry < 1 || retry > 5 {
				t.Fatalf("Retry-After = %d, want the concurrency hint of at most 5s", retry)
			}
			if requests != 1 {
				t.Fatalf("window counts %d requests", requests)
			}
			if got := rhInt(t, resp.Header, limitRequests); got != 10 {
				t.Fatalf("%s = %d", limitRequests, got)
			}
			if got := rhInt(t, resp.Header, remainingRequests); got != 10-requests {
				t.Fatalf("%s = %d, want %d", remainingRequests, got, 10-requests)
			}
			if got := rhInt(t, resp.Header, remainingTokens); got != 100_000-tokens {
				t.Fatalf("%s = %d, want %d", remainingTokens, got, 100_000-tokens)
			}
			// The retry hint is the short wait for a slot, while the window itself
			// resets with the minute, however far off that is.
			for _, name := range []string{resetRequests, resetTokens} {
				rhWantReset(t, surface, name, resp.Header.Get(name), before, after)
			}
		})
	}

	// A budget refuses before the rate window is consulted, and states none.
	t.Run("cost budget", func(t *testing.T) {
		secret := f.budgetKey(t, "spent", "5.00", map[string]any{"requests_per_minute": 5, "tokens_per_minute": 100_000})
		resp := f.open(t, "openai", routeSlug, secret, false)
		body := rhDrain(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests || !bytes.Contains(body, []byte("budget_exhausted")) {
			t.Fatalf("status %d %s", resp.StatusCode, body)
		}
		if got := rhRateHeaderCount(resp.Header); got != 0 {
			t.Fatalf("a budget rejection carried rate-limit headers: %v", resp.Header)
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Fatalf("a budget rejection carried no retry hint: %v", resp.Header)
		}
	})
}

// TestResponseHeadersAreAbsentWithoutAReservation proves a request the limiter
// could not count, and so admitted without a reservation, has no allowance to
// state, while the key's metadata still tells how it was served.
func TestResponseHeadersAreAbsentWithoutAReservation(t *testing.T) {
	f := glSeed(t, glBrokenLimiter(t), limits.FailOpen)
	_, secret := f.key("fail-open", map[string]any{"requests_per_minute": 1, "response_metadata": true})
	for range 2 {
		status, _, header := f.h.gateway("POST", "/v1/chat/completions", secret, map[string]any{
			"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		if got := rhRateHeaderCount(header); got != 0 {
			t.Fatalf("headers of a window nobody counted: %v", header)
		}
		if header.Get("X-Olp-Attempts") != "1" {
			t.Fatalf("metadata missing: %v", header)
		}
	}
}

// rhMetadata is the metadata headers a response carries.
func rhMetadata(header http.Header) map[string]string {
	got := map[string]string{}
	for name, values := range header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-olp-") && lower != "x-olp-delivery-replay" {
			got[name] = strings.Join(values, ",")
		}
	}
	return got
}

// rhCost is what accounting recorded for a request, read once its facts exist.
func rhCost(t *testing.T, f *rhFixture, requestID string) string {
	t.Helper()
	var cost string
	glEventually(t, "accounting to price request "+requestID, func() bool {
		err := f.h.Pool.QueryRow(t.Context(),
			`SELECT COALESCE(sum(estimated_cost)::text, '') FROM olp.attempt_usage_facts WHERE request_id = $1::uuid`, requestID).Scan(&cost)
		return err == nil && cost != ""
	})
	return cost
}

// TestResponseMetadataHeadersFollowTheKeysPolicy holds the opt-in headers to what
// the installation knows: the route revision that was activated, the vendor the
// provider is configured with, the attempts the failover made, and a unary
// response's cost to the cost accounting recorded for it.
func TestResponseMetadataHeadersFollowTheKeysPolicy(t *testing.T) {
	f := rhSeed(t, "response-metadata", glPrice{input: "1", output: "10"})
	glAccounting(t, f.glFixture)
	plain, _ := f.key(t, "plain", nil)
	opted, _ := f.key(t, "opted-in", map[string]any{"response_metadata": true})

	for _, surface := range []string{"openai", "anthropic"} {
		t.Run(surface+"/served directly", func(t *testing.T) {
			// Without the opt-in a response says nothing of how it was served.
			for _, stream := range []bool{false, true} {
				resp := f.open(t, surface, f.slug(surface), plain, stream)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("stream=%v: status %d %s", stream, resp.StatusCode, rhDrain(t, resp))
				}
				rhDrain(t, resp)
				if got := rhMetadata(resp.Header); len(got) != 0 {
					t.Fatalf("stream=%v: a key that did not opt in saw %v", stream, got)
				}
			}

			// A unary response states its cost: 4 tokens in at 1 per million and 6
			// out at 10 per million, which is what accounting later records.
			resp := f.open(t, surface, f.slug(surface), opted, false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("unary: status %d %s", resp.StatusCode, rhDrain(t, resp))
			}
			rhDrain(t, resp)
			want := map[string]string{
				"X-Olp-Attempts": "1", "X-Olp-Route-Revision": f.revision(surface), "X-Olp-Provider": f.vendorID, "X-Olp-Cost": "0.000064",
			}
			if surface == "anthropic" {
				want["X-Olp-Provider"] = f.anthropicVendor
			}
			if got := rhMetadata(resp.Header); len(got) != len(want) {
				t.Fatalf("unary metadata = %v, want %v", got, want)
			}
			for name, value := range want {
				if got := resp.Header.Get(name); got != value {
					t.Fatalf("unary: %s = %q, want %q", name, got, value)
				}
			}
			if recorded := rhCost(t, f, resp.Header.Get("X-Request-Id")); !limSameDecimal(recorded, want["X-Olp-Cost"]) {
				t.Fatalf("the header stated a cost of %s and accounting recorded %s", want["X-Olp-Cost"], recorded)
			}

			// A stream commits before its usage is known: it carries no cost, which
			// remains available from the request history.
			resp = f.open(t, surface, f.slug(surface), opted, true)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stream: status %d %s", resp.StatusCode, rhDrain(t, resp))
			}
			rhDrain(t, resp)
			got := rhMetadata(resp.Header)
			delete(want, "X-Olp-Cost")
			if len(got) != len(want) {
				t.Fatalf("stream metadata = %v, want %v", got, want)
			}
			for name, value := range want {
				if got[name] != value {
					t.Fatalf("stream: %s = %q, want %q", name, got[name], value)
				}
			}
			if recorded := rhCost(t, f, resp.Header.Get("X-Request-Id")); !limSameDecimal(recorded, "0.000064") {
				t.Fatalf("accounting recorded %s for the stream", recorded)
			}
		})
	}

	// The route tries the fixture's provider first and fails over to a second
	// upstream before anything is sent to the caller: the headers count both.
	t.Run("openai/after a failover", func(t *testing.T) {
		f.vendor.fail.Store(true)
		defer f.vendor.fail.Store(false)
		for _, stream := range []bool{false, true} {
			served := f.second.chats.Load()
			resp := f.open(t, "openai", rhFailoverSlug, opted, stream)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stream=%v: status %d %s", stream, resp.StatusCode, rhDrain(t, resp))
			}
			rhDrain(t, resp)
			if f.second.chats.Load() != served+1 {
				t.Fatalf("stream=%v: the second provider did not serve the request", stream)
			}
			got := rhMetadata(resp.Header)
			if got["X-Olp-Attempts"] != "2" || got["X-Olp-Route-Revision"] != f.failoverRevision || got["X-Olp-Provider"] != f.vendorID {
				t.Fatalf("stream=%v: metadata = %v, want 2 attempts on revision %s", stream, got, f.failoverRevision)
			}
			// The failed attempt reported nothing, so the cost is the second's, and
			// accounting records the same for the request: the failed attempt is
			// priced and adds nothing.
			cost, hasCost := got["X-Olp-Cost"]
			if hasCost == stream || hasCost && cost != "0.000064" {
				t.Fatalf("stream=%v: cost = %q (present %v)", stream, cost, hasCost)
			}
			if recorded := rhCost(t, f, resp.Header.Get("X-Request-Id")); !limSameDecimal(recorded, "0.000064") {
				t.Fatalf("stream=%v: the failover cost %s in accounting", stream, recorded)
			}
		}
	})
}

// TestResponseMetadataStatesNoCostForAnUnpricedModel proves a response whose model
// has no price list states none, rather than a cost of nothing.
func TestResponseMetadataStatesNoCostForAnUnpricedModel(t *testing.T) {
	f := rhSeed(t, "response-metadata-unpriced", glPrice{})
	secret, _ := f.key(t, "unpriced", map[string]any{"response_metadata": true})
	resp := f.open(t, "openai", routeSlug, secret, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, rhDrain(t, resp))
	}
	rhDrain(t, resp)
	got := rhMetadata(resp.Header)
	if _, present := got["X-Olp-Cost"]; present || got["X-Olp-Attempts"] != "1" || got["X-Olp-Route-Revision"] != f.routeRevision {
		t.Fatalf("metadata = %v", got)
	}
}

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/tests/fixtures"
)

// minute is the fixed window every reservation of these tests is made in: its ID
// and how long the script says it has left.
const (
	minute      = int64(29_823_060)
	minuteReset = 37_500 * time.Millisecond
)

// window is a Commander that keeps one minute of request and token counters per
// rate key the way the reserve_limits script does, and answers in the reply it
// gives, so that the headers a gateway writes can be read against a real limiter
// without a Valkey. It tells the commands apart by their size: the reservation
// takes twelve arguments, a refund nine, a reconciliation seven and a release five.
// Like the script, it states the allowance only to a reservation that asks for it.
type window struct {
	mu         sync.Mutex
	counters   map[string]*counters
	reserved   []int64           // what each reservation asked for, in order
	asked      map[string][]bool // whether each reservation of a rate key asked for the allowance
	reconciled int               // reconciliations applied
	down       bool              // every command fails, as an outage does
}

// counters are what one rate key's window holds.
type counters struct{ requests, tokens int64 }

func (w *window) of(rateKey string) *counters {
	if w.counters == nil {
		w.counters = map[string]*counters{}
	}
	if w.counters[rateKey] == nil {
		w.counters[rateKey] = &counters{}
	}
	return w.counters[rateKey]
}

func (w *window) Do(_ context.Context, args ...string) (any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.down {
		return nil, errors.New("valkey is down")
	}
	number := func(index int) int64 {
		value, err := strconv.ParseInt(args[index], 10, 64)
		if err != nil {
			panic(err)
		}
		return value
	}
	switch len(args) {
	case 12:
		if w.asked == nil {
			w.asked = map[string][]bool{}
		}
		stated := args[11] == "1"
		w.asked[args[3]] = append(w.asked[args[3]], stated)
		return w.reserve(w.of(args[3]), number(5), number(6), number(7), number(8), stated), nil
	case 9: // refund_limits: window, requests, tokens, lease
		held := w.of(args[3])
		held.requests -= number(6)
		held.tokens -= number(7)
	case 7: // reconcile_limits: window, adjustment, lease
		w.of(args[3]).tokens += number(5)
		w.reconciled++
	}
	return []any{int64(1)}, nil
}

func (w *window) reserve(held *counters, rpm, tpm, tokens, concurrency int64, stated bool) []any {
	remaining := func(limit, used int64) int64 {
		if limit == 0 {
			return 0
		}
		return max(limit-used, 0)
	}
	reset := int64(0)
	if rpm > 0 || tpm > 0 {
		reset = minuteReset.Milliseconds()
	}
	reply := func(status int64, detail string, retry, expiry int64) []any {
		head := []any{int64(2), status, detail, retry, minute, expiry}
		if !stated {
			return head
		}
		return append(head, rpm, remaining(rpm, held.requests), tpm, remaining(tpm, held.tokens), reset)
	}
	switch {
	case rpm > 0 && held.requests >= rpm:
		return reply(0, "rpm", reset, 0)
	case tpm > 0 && held.tokens+tokens > tpm:
		return reply(0, "tpm", reset, 0)
	}
	if rpm > 0 {
		held.requests++
	}
	if tpm > 0 {
		held.tokens += tokens
	}
	w.reserved = append(w.reserved, tokens)
	expiry := int64(0)
	if concurrency > 0 {
		expiry = 1_789_383_605_000
	}
	return reply(1, "ok", 0, expiry)
}

// settled waits for the reconciliations of n requests: they run after the
// response is written, and the next request must not be admitted before them.
func (w *window) settled(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		done := w.reconciled >= n
		w.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d requests were not reconciled within two seconds", n)
}

func (w *window) lastReserved() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reserved[len(w.reserved)-1]
}

// admitThrough puts a limiter over a fresh window in front of the gateway.
func admitThrough(t *testing.T, h *harness) *window {
	t.Helper()
	window := &window{}
	limiter, err := limits.New(window, "olp:test")
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	return window
}

// addKey adds a key with the policy to the harness and returns its secret.
func addKey(h *harness, policy access.KeyPolicy) string {
	secret := "olp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	policy.Scopes = []string{"inference", "models_read"}
	h.rt.keys[secret] = access.Authority{ID: uuid.NewString(), LookupID: "lookup_" + secret[4:20], Policy: policy}
	return secret
}

// headerFixture is the unit harness with a limiter in front of it, providers
// that name their vendor and serve every surface, and a price list.
type headerFixture struct {
	*harness
	window *window
}

const headerFixtureTokens = 5 // what both providers report for a request

// newHeaderFixture serves a, which fails over to b, on the OpenAI, Anthropic and
// Gemini surfaces, unary and streaming.
func newHeaderFixture(t *testing.T) *headerFixture {
	t.Helper()
	h := newHarness(t, Config{})
	window := admitThrough(t, h)
	snapshot := h.rt.release.Snapshot
	price := func(provider runtime.Provider, input, output string) usage.RoutingPrice {
		id := provider.ID
		return usage.RoutingPrice{
			Price: usage.Price{
				ProviderKind: "openai", ProviderID: &id, Model: provider.Capabilities[0].Model, Operation: "generation",
				InputPerMillion: &input, OutputPerMillion: &output, Currency: "USD",
			},
			EffectiveAt: time.Now().Add(-time.Hour),
		}
	}
	var prices []usage.RoutingPrice
	for id, provider := range snapshot.Providers {
		model := provider.Capabilities[0].Model
		provider.Kind, provider.VendorID, provider.Capabilities = "openai", "vendor-"+provider.Name, nil
		for _, surface := range []string{"openai", "anthropic", "gemini"} {
			for _, mode := range []string{"unary", "streaming"} {
				provider.Capabilities = append(provider.Capabilities, runtime.Capability{Model: model, Operation: "generation", Surface: surface, Mode: mode})
			}
		}
		snapshot.Providers[id] = provider
		prices = append(prices, price(provider, "1", "10"))
	}
	h.rt.inputs = &usage.RoutingInputs{Prices: prices, RefreshedAt: time.Now()}
	f := &headerFixture{harness: h, window: window}
	data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	stream := func(model string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Stream bool `json:"stream"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &request)
			if request.Stream {
				_ = testutil.Stream(w, r, data, 64, 0)
				return
			}
			completion(model, answerText)(w, r)
		}
	}
	h.mock.set("a", stream(modelA))
	h.mock.set("b", stream(modelB))
	return f
}

// key adds a key with the policy and returns its secret.
func (f *headerFixture) key(policy access.KeyPolicy) string { return addKey(f.harness, policy) }

func perMinute(n int64) *int64 { return &n }

// send posts one generation request on a surface and returns the response with
// its body read.
func (f *headerFixture) send(surface, key string, stream bool) (*http.Response, []byte) {
	f.t.Helper()
	path, body := "/v1/chat/completions", `{"model":"`+routeSlug+`","max_tokens":20,"stream":`+strconv.FormatBool(stream)+`,"messages":[{"role":"user","content":"hi"}]}`
	switch surface {
	case "anthropic":
		path = "/anthropic/v1/messages"
	case "gemini":
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		path, body = "/gemini/v1beta/models/"+routeSlug+":"+action, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":20}}`
	}
	resp := f.do(f.t.Context(), http.MethodPost, path, key, []byte(body), nil)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// wantHeaders checks that a response carries exactly the headers of a family
// that are named and no other of the family.
func wantHeaders(t *testing.T, resp *http.Response, prefix string, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for name, values := range resp.Header {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			got[name] = strings.Join(values, ",")
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s* headers = %v, want %v", prefix, got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q (all %v)", name, got[name], value, got)
		}
	}
}

// resetOf parses a reset as OpenAI writes one: the notation of a Go duration, in
// whole milliseconds, as the script measured the minute.
func resetOf(t *testing.T, header, value string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(value)
	if err != nil || d.String() != value || d%time.Millisecond != 0 {
		t.Fatalf("%s = %q, which is not a whole number of milliseconds in a duration as OpenAI writes one: %v", header, value, err)
	}
	return d
}

// resetWithin checks that a reset is a duration inside the minute that is left.
func resetWithin(t *testing.T, header, value string) {
	t.Helper()
	if d := resetOf(t, header, value); d > minuteReset || d < minuteReset-10*time.Second {
		t.Fatalf("%s = %s, want what is left of the %s the script measured", header, d, minuteReset)
	}
}

func TestOpenAIRateLimitHeadersCountDownTheKeysWindow(t *testing.T) {
	f := newHeaderFixture(t)
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000)})
	for n := int64(1); n <= 3; n++ {
		resp, body := f.send("openai", key, false)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d %s", n, resp.StatusCode, body)
		}
		// The tokens the window holds are what earlier requests settled at and
		// what this one reserved: the headers state exactly that.
		remaining := 100_000 - headerFixtureTokens*(n-1) - f.window.lastReserved()
		wantHeaders(t, resp, "x-ratelimit-", map[string]string{
			"X-Ratelimit-Limit-Requests":     "5",
			"X-Ratelimit-Remaining-Requests": strconv.FormatInt(5-n, 10),
			"X-Ratelimit-Reset-Requests":     resp.Header.Get("X-Ratelimit-Reset-Requests"),
			"X-Ratelimit-Limit-Tokens":       "100000",
			"X-Ratelimit-Remaining-Tokens":   strconv.FormatInt(remaining, 10),
			"X-Ratelimit-Reset-Tokens":       resp.Header.Get("X-Ratelimit-Reset-Tokens"),
		})
		resetWithin(t, "x-ratelimit-reset-requests", resp.Header.Get("X-Ratelimit-Reset-Requests"))
		resetWithin(t, "x-ratelimit-reset-tokens", resp.Header.Get("X-Ratelimit-Reset-Tokens"))
		wantHeaders(t, resp, "anthropic-", nil)
		wantHeaders(t, resp, "x-olp-attempts", nil)
		f.window.settled(t, int(n))
	}
}

// TestResetCountsDownToTheMomentTheResponseIsWritten proves the reset a response
// states is what is left of the minute when it is committed, not when the request
// was admitted: an upstream that takes a while to answer leaves the key's window
// shorter by that while, for a unary response and for a stream's first frame.
func TestResetCountsDownToTheMomentTheResponseIsWritten(t *testing.T) {
	f := newHeaderFixture(t)
	const wait = 700 * time.Millisecond
	answer := f.mock.handlers["a"]
	f.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		answer(w, r)
	})
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000)})
	for n, stream := range []bool{false, true} {
		resp, body := f.send("openai", key, stream)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v: status %d %s", stream, resp.StatusCode, body)
		}
		for _, header := range []string{headerOpenAIResetRequests, headerOpenAIResetTokens} {
			// The script measured 37.5s and the upstream took 0.7s of it.
			left := resetOf(t, header, resp.Header.Get(header))
			if left > minuteReset-wait || left < minuteReset-10*time.Second {
				t.Errorf("stream=%v: %s = %s, want what is left of the %s the script measured less the %s the upstream took", stream, header, left, minuteReset, wait)
			}
		}
		f.window.settled(t, n+1)
	}
}

func TestAnthropicRateLimitHeadersStateTheEndOfTheWindow(t *testing.T) {
	f := newHeaderFixture(t)
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000)})
	resp, body := f.send("anthropic", key, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	end := time.UnixMilli((minute + 1) * 60_000).UTC().Format(time.RFC3339)
	wantHeaders(t, resp, "anthropic-", map[string]string{
		"Anthropic-Ratelimit-Requests-Limit":     "5",
		"Anthropic-Ratelimit-Requests-Remaining": "4",
		"Anthropic-Ratelimit-Requests-Reset":     end,
		"Anthropic-Ratelimit-Tokens-Limit":       "100000",
		"Anthropic-Ratelimit-Tokens-Remaining":   strconv.FormatInt(100_000-f.window.lastReserved(), 10),
		"Anthropic-Ratelimit-Tokens-Reset":       end,
	})
	wantHeaders(t, resp, "x-ratelimit-", nil)
}

func TestRateLimitHeadersAreSentOnlyForTheDimensionsTheKeyLimits(t *testing.T) {
	f := newHeaderFixture(t)
	for name, tc := range map[string]struct {
		policy access.KeyPolicy
		want   []string
	}{
		"requests": {access.KeyPolicy{RequestsPerMinute: perMinute(7)}, []string{"X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Requests", "X-Ratelimit-Reset-Requests"}},
		"tokens":   {access.KeyPolicy{TokensPerMinute: perMinute(100_000)}, []string{"X-Ratelimit-Limit-Tokens", "X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Reset-Tokens"}},
		// A key bounded only by its concurrency holds a lease, and has no allowance.
		"concurrency": {access.KeyPolicy{MaxConcurrency: perMinute(3)}, nil},
		"unlimited":   {access.KeyPolicy{}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := f.send("openai", f.key(tc.policy), false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d %s", resp.StatusCode, body)
			}
			got := map[string]bool{}
			for header := range resp.Header {
				if strings.HasPrefix(strings.ToLower(header), "x-ratelimit-") {
					got[header] = true
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("rate-limit headers = %v, want %v", got, tc.want)
			}
			for _, header := range tc.want {
				if !got[header] {
					t.Fatalf("rate-limit headers = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestGeminiResponsesCarryNoRateLimitHeaders(t *testing.T) {
	f := newHeaderFixture(t)
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000)})
	resp, body := f.send("gemini", key, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	wantHeaders(t, resp, "x-ratelimit-", nil)
	wantHeaders(t, resp, "anthropic-", nil)
	// The request was counted all the same.
	f.window.mu.Lock()
	defer f.window.mu.Unlock()
	if len(f.window.reserved) != 1 {
		t.Fatalf("limiter reserved %d requests, want 1", len(f.window.reserved))
	}
}

func TestRateLimitedRequestsCarryTheWindowWithTheRetryHint(t *testing.T) {
	f := newHeaderFixture(t)
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(1), TokensPerMinute: perMinute(100_000)})
	if resp, body := f.send("openai", key, false); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	f.window.settled(t, 1)
	reserved := f.window.lastReserved()

	resp, body := f.send("openai", key, false)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "38" {
		t.Fatalf("Retry-After = %q, want the 37.5s the window has left rounded up", got)
	}
	// The refusal reserved nothing: what the window holds is the first request's
	// settled tokens, which the refused one did not add to.
	wantHeaders(t, resp, "x-ratelimit-", map[string]string{
		"X-Ratelimit-Limit-Requests":     "1",
		"X-Ratelimit-Remaining-Requests": "0",
		"X-Ratelimit-Reset-Requests":     resp.Header.Get("X-Ratelimit-Reset-Requests"),
		"X-Ratelimit-Limit-Tokens":       "100000",
		"X-Ratelimit-Remaining-Tokens":   strconv.FormatInt(100_000-headerFixtureTokens, 10),
		"X-Ratelimit-Reset-Tokens":       resp.Header.Get("X-Ratelimit-Reset-Tokens"),
	})
	if reserved <= headerFixtureTokens {
		t.Fatalf("the first request reserved %d tokens, which does not tell reserved and settled apart", reserved)
	}

	// The Anthropic surface states the same refusal in its own headers and error.
	resp, body = f.send("anthropic", key, false)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "38" {
		t.Fatalf("anthropic: status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !bytes.Contains(body, []byte(`"type":"rate_limit_error"`)) {
		t.Fatalf("anthropic: body %s", body)
	}
	wantHeaders(t, resp, "anthropic-", map[string]string{
		"Anthropic-Ratelimit-Requests-Limit":     "1",
		"Anthropic-Ratelimit-Requests-Remaining": "0",
		"Anthropic-Ratelimit-Requests-Reset":     time.UnixMilli((minute + 1) * 60_000).UTC().Format(time.RFC3339),
		"Anthropic-Ratelimit-Tokens-Limit":       "100000",
		"Anthropic-Ratelimit-Tokens-Remaining":   strconv.FormatInt(100_000-headerFixtureTokens, 10),
		"Anthropic-Ratelimit-Tokens-Reset":       time.UnixMilli((minute + 1) * 60_000).UTC().Format(time.RFC3339),
	})

	// Gemini has no such headers, even on a refusal.
	resp, _ = f.send("gemini", key, false)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("gemini: status %d", resp.StatusCode)
	}
	wantHeaders(t, resp, "x-ratelimit-", nil)
	wantHeaders(t, resp, "anthropic-", nil)
}

func TestTokenLimitedRequestsStateTheTokensTheWindowHolds(t *testing.T) {
	f := newHeaderFixture(t)
	// A first request, on a key of its own, tells how much one reserves.
	if resp, body := f.send("openai", f.key(access.KeyPolicy{TokensPerMinute: perMinute(1_000_000)}), false); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	reserved := f.window.lastReserved()
	f.window.settled(t, 1)

	// A window that holds one such request and two tokens over: the second is
	// refused by the token limit, with what the window holds then, which is what
	// the first settled at. The refused request reserved nothing.
	limit := reserved + 2
	key := f.key(access.KeyPolicy{TokensPerMinute: perMinute(limit)})
	if resp, body := f.send("openai", key, false); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status %d %s", resp.StatusCode, body)
	}
	f.window.settled(t, 2)
	resp, _ := f.send("openai", key, false)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d", resp.StatusCode)
	}
	wantHeaders(t, resp, "x-ratelimit-", map[string]string{
		"X-Ratelimit-Limit-Tokens":     strconv.FormatInt(limit, 10),
		"X-Ratelimit-Remaining-Tokens": strconv.FormatInt(limit-headerFixtureTokens, 10),
		"X-Ratelimit-Reset-Tokens":     resp.Header.Get("X-Ratelimit-Reset-Tokens"),
	})
}

func TestStreamsCarryTheAllowanceBeforeTheirFirstFrame(t *testing.T) {
	f := newHeaderFixture(t)
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000)})
	resp, body := f.send("openai", key, true)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !bytes.Contains(body, []byte("[DONE]")) {
		t.Fatalf("status %d type %q body %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if got := resp.Header.Get("X-Ratelimit-Remaining-Requests"); got != "4" {
		t.Fatalf("remaining requests = %q, want 4", got)
	}
	if want := strconv.FormatInt(100_000-f.window.lastReserved(), 10); resp.Header.Get("X-Ratelimit-Remaining-Tokens") != want {
		t.Fatalf("remaining tokens = %q, want %s", resp.Header.Get("X-Ratelimit-Remaining-Tokens"), want)
	}
	resetWithin(t, "x-ratelimit-reset-requests", resp.Header.Get("X-Ratelimit-Reset-Requests"))
}

func TestNoLeaseMeansNoRateLimitHeaders(t *testing.T) {
	f := newHeaderFixture(t)
	f.window.down = true
	f.gateway.Admission = NewAdmission(f.gateway.Admission.limiter, func() limits.OutagePolicy { return limits.FailOpen }, slog.New(slog.DiscardHandler))
	// The limiter cannot answer and the installation fails open: the request is
	// admitted without a reservation, and there is no allowance to state.
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(5), ResponseMetadata: true})
	resp, body := f.send("openai", key, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	wantHeaders(t, resp, "x-ratelimit-", nil)
	// Metadata does not depend on a reservation.
	if resp.Header.Get("X-Olp-Attempts") != "1" {
		t.Fatalf("headers %v", resp.Header)
	}
}

func metadataOf(resp *http.Response) map[string]string {
	got := map[string]string{}
	for name, values := range resp.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-olp-") && name != "X-Olp-Delivery-Replay" {
			got[name] = strings.Join(values, ",")
		}
	}
	return got
}

func TestMetadataHeadersAppearOnlyWhenTheKeyOptsIn(t *testing.T) {
	f := newHeaderFixture(t)
	route := f.rt.release.Snapshot.Routes[routeSlug]
	for _, surface := range []string{"openai", "anthropic", "gemini"} {
		t.Run(surface, func(t *testing.T) {
			resp, body := f.send(surface, f.key(access.KeyPolicy{}), false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d %s", resp.StatusCode, body)
			}
			if got := metadataOf(resp); len(got) != 0 {
				t.Fatalf("a key that did not opt in saw %v", got)
			}
			resp, body = f.send(surface, f.key(access.KeyPolicy{ResponseMetadata: true}), false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d %s", resp.StatusCode, body)
			}
			// Two input tokens at one unit per million and three output tokens at ten.
			want := map[string]string{
				"X-Olp-Attempts": "1", "X-Olp-Route-Revision": route.RevisionID, "X-Olp-Provider": "vendor-a", "X-Olp-Cost": "0.000032",
			}
			if got := metadataOf(resp); len(got) != len(want) {
				t.Fatalf("metadata = %v, want %v", got, want)
			}
			for name, value := range want {
				if got := resp.Header.Get(name); got != value {
					t.Fatalf("%s = %q, want %q (headers %v)", name, got, value, metadataOf(resp))
				}
			}
		})
	}
}

// The headers are the one place the gateway itself names a provider, and a key
// that did not opt in gets none. What an upstream says of itself is not the
// gateway's to scrub: the message of an upstream rejection is relayed with
// credential values redacted, whatever the key's policy, and the documentation
// says so.
func TestAnUpstreamRejectionIsRelayedWhateverTheKeysPolicy(t *testing.T) {
	f := newHeaderFixture(t)
	f.mock.set("a", status(http.StatusBadRequest, `{"error":{"message":"vendor-a rejected the request: model gpt-upstream-secret is overloaded","type":"invalid_request_error"}}`))
	for _, opted := range []bool{false, true} {
		resp, body := f.send("openai", f.key(access.KeyPolicy{ResponseMetadata: opted}), false)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"upstream_rejected"`) ||
			!strings.Contains(string(body), "vendor-a rejected the request: model gpt-upstream-secret is overloaded") {
			t.Fatalf("opted in %v: status %d body %s, want the upstream's message relayed", opted, resp.StatusCode, body)
		}
		// Metadata belongs to the successful responses of a key that opted in.
		if got := metadataOf(resp); len(got) != 0 {
			t.Fatalf("opted in %v: the rejection carries %v", opted, got)
		}
	}
}

func TestMetadataStatesTheAttemptsAFailoverMade(t *testing.T) {
	f := newHeaderFixture(t)
	route := f.rt.release.Snapshot.Routes[routeSlug]
	down := status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`)
	f.mock.set("a", down)
	key := f.key(access.KeyPolicy{ResponseMetadata: true, RequestsPerMinute: perMinute(5)})

	resp, body := f.send("openai", key, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	// The first attempt failed after it was sent, so the request made two, and b
	// answered. Its cost is b's alone: nothing was reported for a.
	wantMetadata := map[string]string{
		"X-Olp-Attempts": "2", "X-Olp-Route-Revision": route.RevisionID, "X-Olp-Provider": "vendor-b", "X-Olp-Cost": "0.000032",
	}
	got := metadataOf(resp)
	if len(got) != len(wantMetadata) {
		t.Fatalf("metadata = %v, want %v", got, wantMetadata)
	}
	for name, value := range wantMetadata {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q", name, got[name], value)
		}
	}

	// A stream commits before its attempt's fact exists, and still reports the
	// attempts made so far and the vendor that is streaming. It has no cost yet.
	resp, body = f.send("openai", key, true)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("[DONE]")) {
		t.Fatalf("stream: status %d %s", resp.StatusCode, body)
	}
	got = metadataOf(resp)
	if got["X-Olp-Attempts"] != "2" || got["X-Olp-Provider"] != "vendor-b" || got["X-Olp-Route-Revision"] != route.RevisionID {
		t.Fatalf("stream metadata = %v", got)
	}
	if _, present := got["X-Olp-Cost"]; present || len(got) != 3 {
		t.Fatalf("a stream states a cost it cannot know yet: %v", got)
	}
	// The envelope agrees with what the headers said.
	if env := f.sink.last(t); len(env.Attempts) != 2 || env.Attempts[1].VendorID != "vendor-b" {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

func TestMetadataOmitsWhatTheGatewayCannotState(t *testing.T) {
	f := newHeaderFixture(t)
	// No price list at all: nothing a request does is priced, and a cost is not
	// invented for it.
	f.rt.inputs = nil
	key := f.key(access.KeyPolicy{ResponseMetadata: true})
	resp, body := f.send("openai", key, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if _, present := metadataOf(resp)["X-Olp-Cost"]; present || resp.Header.Get("X-Olp-Attempts") != "1" {
		t.Fatalf("unpriced response carried %v", metadataOf(resp))
	}

	// A provider with no vendor names none.
	snapshot := f.rt.release.Snapshot
	for id, provider := range snapshot.Providers {
		provider.VendorID = ""
		snapshot.Providers[id] = provider
	}
	resp, _ = f.send("openai", key, false)
	if _, present := metadataOf(resp)["X-Olp-Provider"]; present || resp.Header.Get("X-Olp-Attempts") != "1" {
		t.Fatalf("a provider without a vendor was named: %v", metadataOf(resp))
	}
}

// TestMediaResponsesCarryTheAllowanceAndMetadata proves the media endpoints, which
// commit their responses on paths of their own, write the headers too: a buffered
// body, and a stream on its first frame. A key that did not opt in is told its
// allowance on both, and nothing of how it was served.
func TestMediaResponsesCarryTheAllowanceAndMetadata(t *testing.T) {
	h := newMediaHarness(t)
	window := admitThrough(t, h)
	snapshot := h.rt.release.Snapshot
	for id, provider := range snapshot.Providers {
		provider.VendorID = "vendor-" + provider.Name
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes[routeSlug]
	key := addKey(h, access.KeyPolicy{RequestsPerMinute: perMinute(10), TokensPerMinute: perMinute(1_000_000), ResponseMetadata: true})
	plain := addKey(h, access.KeyPolicy{RequestsPerMinute: perMinute(10), TokensPerMinute: perMinute(1_000_000)})
	metadata := map[string]string{"X-Olp-Attempts": "1", "X-Olp-Route-Revision": route.RevisionID, "X-Olp-Provider": "vendor-a"}

	speech := func(key string) *http.Response {
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/mpeg")
			io.WriteString(w, "audio-payload")
		})
		resp := h.do(t.Context(), "POST", "/v1/audio/speech", key, []byte(`{"model":"team-chat","input":"hello","voice":"alloy"}`), nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("speech: status %d", resp.StatusCode)
		}
		return resp
	}
	image := func(key string) *http.Response {
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
		})
		resp := h.do(t.Context(), "POST", "/v1/images/generations", key, []byte(`{"model":"team-chat","prompt":"photo","stream":true}`), nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "image_generation.completed") {
			t.Fatalf("stream: status %d %s", resp.StatusCode, body)
		}
		return resp
	}

	// The speech reported no usage, so nothing is known to have cost anything.
	resp := speech(key)
	wantHeaders(t, resp, "x-olp-", metadata)
	if got := resp.Header.Get("X-Ratelimit-Remaining-Requests"); got != "9" {
		t.Fatalf("speech: remaining requests = %q, want 9", got)
	}
	if got := resp.Header.Get("X-Ratelimit-Limit-Tokens"); got != "1000000" {
		t.Fatalf("speech: token limit = %q", got)
	}
	resp = image(key)
	wantHeaders(t, resp, "x-olp-", metadata)
	if got := resp.Header.Get("X-Ratelimit-Remaining-Requests"); got != "8" {
		t.Fatalf("stream: remaining requests = %q, want 8", got)
	}

	// A key that did not opt in has the allowance of the same responses, which
	// proves they went through the writer, and none of the provider's identity.
	for name, send := range map[string]func(string) *http.Response{"speech": speech, "stream": image} {
		resp := send(plain)
		wantHeaders(t, resp, "x-olp-", nil)
		if got := resp.Header.Get("X-Ratelimit-Limit-Requests"); got != "10" {
			t.Fatalf("%s: request limit = %q, want 10 (headers %v)", name, got, resp.Header)
		}
	}
	if len(window.reserved) != 4 {
		t.Fatalf("limiter reserved %d requests, want 4", len(window.reserved))
	}
}

// TestEveryHeaderOfAResponseFitsTogether proves the most a response can carry, the
// six rate-limit headers of a key limited on both dimensions beside the four
// metadata headers of one that opted in, are all written.
func TestEveryHeaderOfAResponseFitsTogether(t *testing.T) {
	f := newHeaderFixture(t)
	key := f.key(access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000), ResponseMetadata: true})
	resp, body := f.send("openai", key, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	for _, name := range []string{
		headerOpenAILimitRequests, headerOpenAIRemainingRequests, headerOpenAIResetRequests,
		headerOpenAILimitTokens, headerOpenAIRemainingTokens, headerOpenAIResetTokens,
		headerAttempts, headerRouteRevision, headerProvider, headerCost,
	} {
		if resp.Header.Get(name) == "" {
			t.Errorf("%s is missing from %v", name, resp.Header)
		}
	}
}

// TestOnlyTheKeysReservationAsksForTheAllowance proves a request through
// providers that have quotas of their own asks the rate script to state an
// allowance for the key's reservation alone, and only where the key limits
// requests or tokens: nothing reports on a provider's connection or credential
// quota, or on a key that is bound by its concurrency only, and a reply that
// stated one would be decoded for nothing on every attempt.
func TestOnlyTheKeysReservationAsksForTheAllowance(t *testing.T) {
	f := newHeaderFixture(t)
	for id, provider := range f.rt.release.Snapshot.Providers {
		provider.Limits = &runtime.Limits{RequestsPerMinute: perMinute(1000), TokensPerMinute: perMinute(10_000_000), MaxConcurrency: perMinute(10)}
		for index := range provider.Slots {
			provider.Slots[index].RequestsPerMinute = perMinute(1000)
			provider.Slots[index].TokensPerMinute = perMinute(10_000_000)
		}
		f.rt.release.Snapshot.Providers[id] = provider
	}
	for name, tc := range map[string]struct {
		policy     access.KeyPolicy
		wantStated bool
	}{
		"requests and tokens": {access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000)}, true},
		"requests":            {access.KeyPolicy{RequestsPerMinute: perMinute(5)}, true},
		"tokens":              {access.KeyPolicy{TokensPerMinute: perMinute(100_000)}, true},
		"concurrency":         {access.KeyPolicy{MaxConcurrency: perMinute(3)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f.window.mu.Lock()
			f.window.asked = nil
			f.window.mu.Unlock()
			resp, body := f.send("openai", f.key(tc.policy), false)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d %s", resp.StatusCode, body)
			}
			if got := resp.Header.Get("X-Ratelimit-Limit-Requests") != "" || resp.Header.Get("X-Ratelimit-Limit-Tokens") != ""; got != tc.wantStated {
				t.Fatalf("rate-limit headers present = %t, want %t (headers %v)", got, tc.wantStated, resp.Header)
			}
			f.window.mu.Lock()
			defer f.window.mu.Unlock()
			seen := map[string]int{}
			for rateKey, asks := range f.window.asked {
				var scope string
				switch {
				case strings.Contains(rateKey, "{lookup_"):
					scope = "key"
				case strings.Contains(rateKey, "{pc_"):
					scope = "connection"
				case strings.Contains(rateKey, "{ps_"):
					scope = "credential"
				default:
					t.Fatalf("a reservation of the unexpected rate key %q", rateKey)
				}
				for _, stated := range asks {
					seen[scope]++
					if want := scope == "key" && tc.wantStated; stated != want {
						t.Errorf("the %s reservation asked for the allowance = %t, want %t", scope, stated, want)
					}
				}
			}
			if seen["key"] != 1 || seen["connection"] != 1 || seen["credential"] != 1 {
				t.Fatalf("reservations by scope = %v, want one of the key, the connection and the credential", seen)
			}
		})
	}
}

// keyAsks is what the key's own reservations asked of the limiter: whether each
// stated the allowance. The reservations of a provider's connection and
// credential are the ones of other rate keys.
func (w *window) keyAsks() []bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	var asks []bool
	for rateKey, asked := range w.asked {
		if strings.Contains(rateKey, "{lookup_") {
			asks = append(asks, asked...)
		}
	}
	return asks
}

// TestTheAllowanceIsAskedForOnlyWhereItIsReported holds the key's reservation to
// asking the rate script for its allowance when the surface the caller speaks has
// headers to carry it, which are the OpenAI and Anthropic surfaces', and not
// otherwise: a reply that states one costs a request a dozen allocations to build,
// and a request on the Gemini, Bedrock or native surface would discard it.
func TestTheAllowanceIsAskedForOnlyWhereItIsReported(t *testing.T) {
	policy := access.KeyPolicy{RequestsPerMinute: perMinute(5), TokensPerMinute: perMinute(100_000)}
	authority := access.Authority{ID: uuid.NewString(), LookupID: "lookup_allowance", Policy: policy}
	t.Run("at admission", func(t *testing.T) {
		for _, surface := range []string{"openai", "anthropic", "gemini", "bedrock", "native"} {
			w := &window{}
			limiter, err := limits.New(w, "olp:test")
			if err != nil {
				t.Fatal(err)
			}
			admission := NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, quiet)
			lease, e := admission.reserveKey(t.Context(), authority, surface, 10, time.Minute)
			if e != nil || lease == nil {
				t.Fatalf("%s: lease %v, error %v", surface, lease, e)
			}
			want := rateHeadersOf(surface) != nil
			if asks := w.keyAsks(); len(asks) != 1 || asks[0] != want {
				t.Errorf("%s: the key's reservation asked for the allowance %v, want %v", surface, asks, want)
			}
			if got := lease.RateState().Limited(); got != want {
				t.Errorf("%s: the lease holds an allowance = %v, want %v", surface, got, want)
			}
		}
	})
	t.Run("a rejection", func(t *testing.T) {
		for _, surface := range []string{"openai", "gemini"} {
			w := &window{}
			limiter, err := limits.New(w, "olp:test")
			if err != nil {
				t.Fatal(err)
			}
			admission := NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, quiet)
			for range 5 {
				if _, e := admission.reserveKey(t.Context(), authority, surface, 10, time.Minute); e != nil {
					t.Fatalf("%s: %v", surface, e)
				}
			}
			_, e := admission.reserveKey(t.Context(), authority, surface, 10, time.Minute)
			if e == nil || e.Status != http.StatusTooManyRequests {
				t.Fatalf("%s: the sixth request was admitted: %v", surface, e)
			}
			if got := e.rate.Limited(); got != (rateHeadersOf(surface) != nil) {
				t.Errorf("%s: the rejection holds an allowance = %v", surface, got)
			}
		}
	})
	t.Run("through the gateway", func(t *testing.T) {
		f := newHeaderFixture(t)
		for _, tc := range []struct {
			surface string
			stream  bool
		}{{"openai", false}, {"openai", true}, {"anthropic", false}, {"anthropic", true}, {"gemini", false}, {"gemini", true}} {
			f.window.mu.Lock()
			f.window.asked = nil
			f.window.mu.Unlock()
			resp, body := f.send(tc.surface, f.key(policy), tc.stream)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s stream=%v: status %d %s", tc.surface, tc.stream, resp.StatusCode, body)
			}
			want := tc.surface != "gemini"
			if asks := f.window.keyAsks(); len(asks) != 1 || asks[0] != want {
				t.Errorf("%s stream=%v: the key's reservation asked for the allowance %v, want %v", tc.surface, tc.stream, asks, want)
			}
			// What was asked is what the response says.
			prefix := map[string]string{"openai": "x-ratelimit-", "anthropic": "anthropic-ratelimit-"}[tc.surface]
			if tc.surface == "gemini" {
				wantHeaders(t, resp, "x-ratelimit-", nil)
				wantHeaders(t, resp, "anthropic-ratelimit-", nil)
			} else if got := metadataOfPrefix(resp, prefix); len(got) != 6 {
				t.Errorf("%s stream=%v: %d %s headers, want 6: %v", tc.surface, tc.stream, len(got), prefix, got)
			}
		}
	})
}

// metadataOfPrefix is the headers of a response that start with a prefix.
func metadataOfPrefix(resp *http.Response, prefix string) map[string]string {
	got := map[string]string{}
	for name, values := range resp.Header {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			got[name] = strings.Join(values, ",")
		}
	}
	return got
}

// A job call on a video has no prompt, and reserves what a call on any other
// resource does. A key with a token limit refuses a reservation of nothing, as
// invalid, and the refusal read as an outage, so such a key could create a job and
// never read it back, list its jobs, download or delete one.
func TestVideoJobCallsAreAdmittedForAKeyWithATokenLimit(t *testing.T) {
	policy := access.KeyPolicy{RequestsPerMinute: perMinute(10), TokensPerMinute: perMinute(1_000_000)}
	for _, family := range []openai.Family{openai.FamilyVideoList, openai.FamilyVideoGet, openai.FamilyVideoContent, openai.FamilyVideoDelete} {
		t.Run(string(family), func(t *testing.T) {
			w := &window{}
			limiter, err := limits.New(w, "olp:test")
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{Admission: NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, quiet), log: quiet}
			x := &execution{family: family}
			_, complete, e := s.admitVideoRequest(t.Context(), x, admissionAuthority(policy))
			if e != nil {
				t.Fatalf("a job call was refused for a key with a token limit: %+v", e)
			}
			defer complete()
			if got := w.lastReserved(); got != resourceEstimate {
				t.Errorf("the call reserved %d tokens, want the %d of a call on a resource", got, resourceEstimate)
			}
			// The call reports the key's allowance as every other response does.
			header := http.Header{}
			x.responseHeaders(header, false)
			if header.Get("X-Ratelimit-Limit-Requests") != "10" || header.Get("X-Ratelimit-Limit-Tokens") != "1000000" || header.Get("X-Ratelimit-Remaining-Requests") != "9" {
				t.Errorf("the response carries %v, want the allowance of the key", header)
			}
		})
	}
}

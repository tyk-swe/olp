package gateway

import (
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

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
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

// newHeaderFixture serves a, which fails over to b, on the OpenAI, Anthropic and
// Gemini surfaces, unary and streaming.
func newHeaderFixture(t *testing.T) *headerFixture {
	t.Helper()
	h := newHarness(t, Config{})
	window := admitThrough(t, h)
	snapshot := h.rt.release.Snapshot
	price := func(provider routing.Provider, input, output string) usage.RoutingPrice {
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
				provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: model, Operation: "generation", Surface: surface, Mode: mode})
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

// The tokens the window holds are what earlier requests settled at and
// what this one reserved: the headers state exactly that.

// The script measured 37.5s and the upstream took 0.7s of it.

// A key bounded only by its concurrency holds a lease, and has no allowance.

// The request was counted all the same.

// The refusal reserved nothing: what the window holds is the first request's
// settled tokens, which the refused one did not add to.

// The Anthropic surface states the same refusal in its own headers and error.

// Gemini has no such headers, even on a refusal.

// A first request, on a key of its own, tells how much one reserves.

// A window that holds one such request and two tokens over: the second is
// refused by the token limit, with what the window holds then, which is what
// the first settled at. The refused request reserved nothing.

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

// Two input tokens at one unit per million and three output tokens at ten.

// Metadata belongs to the successful responses of a key that opted in.

// The first attempt failed after it was sent, so the request made two, and b
// answered. Its cost is b's alone: nothing was reported for a.

// A stream commits before its attempt's fact exists, and still reports the
// attempts made so far and the vendor that is streaming. It has no cost yet.

// The envelope agrees with what the headers said.

// No price list at all: nothing a request does is priced, and a cost is not
// invented for it.

// A provider with no vendor names none.

// The speech reported no usage, so nothing is known to have cost anything.

// A key that did not opt in has the allowance of the same responses, which
// proves they went through the writer, and none of the provider's identity.

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

// The call reports the key's allowance as every other response does.

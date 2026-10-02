package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// TestAppendResetDurationMatchesDurationString proves the allocation-free writer
// prints a reset exactly as time.Duration does, which is the notation OpenAI's
// reset headers use, for every millisecond a window can hold and past it.
func TestAppendResetDurationMatchesDurationString(t *testing.T) {
	for ms := int64(0); ms <= 130_000; ms++ {
		d := time.Duration(ms) * time.Millisecond
		if got := string(appendResetDuration(nil, d)); got != d.String() {
			t.Fatalf("%dms is written %q, want %q", ms, got, d.String())
		}
	}
	for _, d := range []time.Duration{
		-time.Second, 0, time.Nanosecond, 1500 * time.Microsecond, 59*time.Second + 999*time.Millisecond + time.Nanosecond,
		time.Hour - time.Millisecond, time.Hour, 25*time.Hour + 3*time.Minute + 7*time.Second + 20*time.Millisecond,
	} {
		want := d.String()
		if d <= 0 {
			want = "0s"
		}
		if got := string(appendResetDuration(nil, d)); got != want {
			t.Fatalf("%v is written %q, want %q", d, got, want)
		}
	}
	// The literal forms the OpenAI documentation shows.
	for d, want := range map[time.Duration]string{
		20 * time.Millisecond: "20ms", time.Second: "1s", 8640 * time.Millisecond: "8.64s", time.Minute: "1m0s",
		90 * time.Second: "1m30s", 59999 * time.Millisecond: "59.999s",
	} {
		if got := string(appendResetDuration(nil, d)); got != want {
			t.Fatalf("%v is written %q, want %q", d, got, want)
		}
	}
}

// TestHeaderNamesAreStoredCanonically proves every name this file writes is the
// canonical form: it is stored in the header map as written, so a name that was
// not would be invisible to a lookup and sent in a case no one asked for.
func TestHeaderNamesAreStoredCanonically(t *testing.T) {
	names := []string{headerAttempts, headerRouteRevision, headerProvider, headerCost}
	for _, family := range []*rateHeaders{&openAIRateHeaders, &anthropicRateHeaders} {
		names = append(names, family.limitRequests, family.remainingRequests, family.resetRequests,
			family.limitTokens, family.remainingTokens, family.resetTokens)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if name != http.CanonicalHeaderKey(name) {
			t.Errorf("%q is not canonical, which is %q", name, http.CanonicalHeaderKey(name))
		}
		if seen[name] {
			t.Errorf("%q is named twice", name)
		}
		seen[name] = true
	}
}

// rateAllowance is a key's allowance a quarter of the way through a window that
// ends at a known instant.
func rateAllowance(requests, tokens bool) limits.RateState {
	state := limits.RateState{ResetAfter: 37500 * time.Millisecond, ResetAt: time.Date(2026, 10, 2, 9, 31, 0, 0, time.UTC)}
	if requests {
		state.RequestLimit, state.RequestRemaining = 60, 59
	}
	if tokens {
		state.TokenLimit, state.TokenRemaining = 150000, 149984
	}
	return state
}

func TestRateHeadersFollowTheSurfaceAndTheDimensionsTheKeyLimits(t *testing.T) {
	for _, tc := range []struct {
		name             string
		surface          string
		requests, tokens bool
		want             map[string]string
	}{
		{
			name: "openai both", surface: "openai", requests: true, tokens: true,
			want: map[string]string{
				"X-Ratelimit-Limit-Requests": "60", "X-Ratelimit-Remaining-Requests": "59", "X-Ratelimit-Reset-Requests": "37.5s",
				"X-Ratelimit-Limit-Tokens": "150000", "X-Ratelimit-Remaining-Tokens": "149984", "X-Ratelimit-Reset-Tokens": "37.5s",
			},
		},
		{
			name: "openai requests only", surface: "openai", requests: true,
			want: map[string]string{
				"X-Ratelimit-Limit-Requests": "60", "X-Ratelimit-Remaining-Requests": "59", "X-Ratelimit-Reset-Requests": "37.5s",
			},
		},
		{
			name: "openai tokens only", surface: "openai", tokens: true,
			want: map[string]string{
				"X-Ratelimit-Limit-Tokens": "150000", "X-Ratelimit-Remaining-Tokens": "149984", "X-Ratelimit-Reset-Tokens": "37.5s",
			},
		},
		{
			name: "anthropic both", surface: "anthropic", requests: true, tokens: true,
			want: map[string]string{
				"Anthropic-Ratelimit-Requests-Limit": "60", "Anthropic-Ratelimit-Requests-Remaining": "59", "Anthropic-Ratelimit-Requests-Reset": "2026-10-02T09:31:00Z",
				"Anthropic-Ratelimit-Tokens-Limit": "150000", "Anthropic-Ratelimit-Tokens-Remaining": "149984", "Anthropic-Ratelimit-Tokens-Reset": "2026-10-02T09:31:00Z",
			},
		},
		{
			name: "anthropic tokens only", surface: "anthropic", tokens: true,
			want: map[string]string{
				"Anthropic-Ratelimit-Tokens-Limit": "150000", "Anthropic-Ratelimit-Tokens-Remaining": "149984", "Anthropic-Ratelimit-Tokens-Reset": "2026-10-02T09:31:00Z",
			},
		},
		// Gemini and Bedrock SDKs read no such headers, and nothing reads a native
		// operation's: none are sent.
		{name: "gemini", surface: "gemini", requests: true, tokens: true},
		{name: "bedrock", surface: "bedrock", requests: true, tokens: true},
		{name: "native", surface: "native", requests: true, tokens: true},
		{name: "unknown", surface: "", requests: true, tokens: true},
		// A key that limits neither dimension has no allowance to state.
		{name: "openai unlimited", surface: "openai"},
		{name: "anthropic unlimited", surface: "anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			setRateLimitHeaders(h, tc.surface, rateAllowance(tc.requests, tc.tokens))
			if len(h) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", h, tc.want)
			}
			for name, want := range tc.want {
				if got := h.Values(name); len(got) != 1 || got[0] != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// TestRateHeadersOfARejectionStateWhatTheWindowHolds proves the allowance a 429
// carries reaches the response through the error, whichever handler wrote it, and
// that an error that is not the key's own rate limit carries none.
func TestRateHeadersOfARejectionStateWhatTheWindowHolds(t *testing.T) {
	state := rateAllowance(true, true)
	state.RequestRemaining = 0
	for _, tc := range []struct {
		surface string
		want    string
	}{{"openai", "X-Ratelimit-Remaining-Requests"}, {"anthropic", "Anthropic-Ratelimit-Requests-Remaining"}, {"gemini", ""}, {"bedrock", ""}} {
		e := rateLimited(limits.DimensionRequests, 38*time.Second, false)
		e.rate = state
		rec := httptest.NewRecorder()
		writeSurfaceError(rec, e, tc.surface)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "38" {
			t.Fatalf("%s: status %d, Retry-After %q", tc.surface, rec.Code, rec.Header().Get("Retry-After"))
		}
		if tc.want == "" {
			for name := range rec.Header() {
				if strings.Contains(strings.ToLower(name), "ratelimit") {
					t.Errorf("%s: carries %s", tc.surface, name)
				}
			}
			continue
		}
		if got := rec.Header().Get(tc.want); got != "0" {
			t.Errorf("%s: %s = %q, want 0", tc.surface, tc.want, got)
		}
	}
	// Quotas of a provider and of a cost budget are not the caller's allowance.
	for name, e := range map[string]*Error{
		"cost budget":      rateLimited(limits.DimensionDailyCost, time.Second, true),
		"connection quota": (&attemptFailure{class: classRateLimit, quota: quotaConnection, retryAfter: time.Second}).toError(),
		"slot quota":       (&attemptFailure{class: classRateLimit, quota: quotaSlot, retryAfter: time.Second}).toError(),
		"upstream":         (&attemptFailure{class: classRateLimit, retryAfter: time.Second}).toError(),
		"overloaded":       overloaded,
	} {
		rec := httptest.NewRecorder()
		writeSurfaceError(rec, e, "openai")
		for header := range rec.Header() {
			if strings.Contains(strings.ToLower(header), "ratelimit") {
				t.Errorf("%s: carries %s", name, header)
			}
		}
	}
}

// TestResponseHeadersCostNothingWhereNothingIsConfigured proves the budget of a
// feature that is off: a request whose key limits nothing and does not opt in adds
// no allocation, and the headers of one that does add two however many there are.
func TestResponseHeadersCostNothingWhereNothingIsConfigured(t *testing.T) {
	sink := make(http.Header, 16)
	for name, x := range map[string]*execution{
		"no lease":       {family: openai.FamilyChat},
		"no attempt yet": {family: openai.FamilyChat, responseMetadata: true},
		"gemini":         {family: openai.FamilyGemini, attemptCount: 1},
		"metadata off":   {family: openai.FamilyChat, attemptCount: 2, attemptVendor: "vendor"},
	} {
		if got := testing.AllocsPerRun(100, func() { x.responseHeaders(sink, false) }); got != 0 {
			t.Errorf("%s: %v allocations, want none", name, got)
		}
		if len(sink) != 0 {
			t.Errorf("%s: added %v", name, sink)
		}
	}
	state := rateAllowance(true, true)
	if got := testing.AllocsPerRun(100, func() {
		clear(sink)
		setRateLimitHeaders(sink, "openai", state)
	}); got > 2 {
		t.Errorf("six rate-limit headers cost %v allocations, want at most two", got)
	}
	if got := testing.AllocsPerRun(100, func() {
		clear(sink)
		setRateLimitHeaders(sink, "gemini", state)
		setRateLimitHeaders(sink, "openai", limits.RateState{})
	}); got != 0 {
		t.Errorf("a surface with no rate-limit headers and a key with no limits cost %v allocations", got)
	}
}

// reservedLease is the reservation of a key limited per minute as the limiter
// answers it, with the allowance and the reset a response states.
func reservedLease(t *testing.T, requests, tokens int64) *limits.Lease {
	t.Helper()
	limiter, err := limits.New(&window{}, "olp:test")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := limiter.Reserve(t.Context(), limits.Request{
		CostOwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", LookupID: "lookup_allocations", RequestsPerMinute: &requests, TokensPerMinute: &tokens,
		RequestedTokens: headerFixtureTokens, LeaseTTL: 5 * time.Second, ReportRate: true,
	})
	if err != nil || !lease.RateState().Limited() {
		t.Fatalf("Reserve: %+v, %v", lease.RateState(), err)
	}
	return lease
}

// TestSuccessHeadersAllocateTwiceAtMostHoweverManyThereAre guards the path every
// success response of a limited or opted-in key runs: the allowance of a real
// reservation and the metadata of an attempt are written with the two allocations
// a response's headers cost, the one string they share and the lists the header
// map holds. A formatting call that allocates for each value would show here.
func TestSuccessHeadersAllocateTwiceAtMostHoweverManyThereAre(t *testing.T) {
	both := reservedLease(t, 60, 150_000)
	route := &runtime.Route{RevisionID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"}
	sink := make(http.Header, 16)
	for _, tc := range []struct {
		name     string
		x        *execution
		streamed bool
		want     int // headers written
	}{
		{"openai allowance", &execution{family: openai.FamilyChat, lease: both}, false, 6},
		{"anthropic allowance", &execution{family: openai.FamilyAnthropic, lease: both}, false, 6},
		{"metadata of a stream", &execution{family: openai.FamilyChat, responseMetadata: true, attemptCount: 2, attemptVendor: "vendor-b", route: route}, true, 3},
		{"openai allowance and metadata of a stream", &execution{family: openai.FamilyChat, lease: both, responseMetadata: true, attemptCount: 2, attemptVendor: "vendor-b", route: route}, true, 9},
		{"anthropic allowance and metadata of a stream", &execution{family: openai.FamilyAnthropic, lease: both, responseMetadata: true, attemptCount: 1, attemptVendor: "vendor-a", route: route}, true, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(100, func() {
				clear(sink)
				tc.x.responseHeaders(sink, tc.streamed)
			}); got > 2 {
				t.Errorf("%v allocations, want at most two", got)
			}
			clear(sink)
			tc.x.responseHeaders(sink, tc.streamed)
			if len(sink) != tc.want {
				t.Errorf("wrote %d headers, want %d: %v", len(sink), tc.want, sink)
			}
		})
	}
}

// TestVideoCallsStateTheAttemptsTheyMade proves what a call on a video job and a
// list of them tell a key that opted in: a call is its one attempt, and a list is
// all of its polls, naming a provider only when one served every poll.
func TestVideoCallsStateTheAttemptsTheyMade(t *testing.T) {
	polled := func(vendors ...string) []AttemptFact {
		facts := make([]AttemptFact, len(vendors))
		for i, vendor := range vendors {
			facts[i] = AttemptFact{VendorID: vendor}
		}
		return facts
	}
	for _, tc := range []struct {
		name   string
		facts  []AttemptFact
		count  int
		vendor string
	}{
		{"a list that polled nothing made no attempt", nil, 0, ""},
		{"one poll", polled("vendor-a"), 1, "vendor-a"},
		{"polls of one vendor", polled("vendor-a", "vendor-a", "vendor-a"), 3, "vendor-a"},
		{"polls of two vendors name neither", polled("vendor-a", "vendor-a", "vendor-b"), 3, ""},
		{"a poll that reached no provider leaves the vendor unknown", polled("vendor-a", ""), 2, ""},
		{"polls of a provider without a vendor name none", polled("", ""), 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &execution{facts: tc.facts, attemptCount: 99, attemptVendor: "stale"}
			x.notePolls()
			if x.attemptCount != tc.count || x.attemptVendor != tc.vendor {
				t.Fatalf("a list states %d attempts on %q, want %d on %q", x.attemptCount, x.attemptVendor, tc.count, tc.vendor)
			}
		})
	}

	x := &execution{}
	x.noteJobAttempt(AttemptFact{VendorID: "vendor-a"})
	if x.attemptCount != 1 || x.attemptVendor != "vendor-a" || len(x.facts) != 1 {
		t.Fatalf("a call states %d attempts on %q with %d facts, want its one", x.attemptCount, x.attemptVendor, len(x.facts))
	}
}

// TestCORSExposesEveryHeaderTheGatewayAdds proves a browser SDK can read the
// headers this file writes, and the ones the gateway already exposed.
func TestCORSExposesEveryHeaderTheGatewayAdds(t *testing.T) {
	s := &Server{}
	s.cfg.CORSAllowedOrigins = []string{"https://console.example"}
	for _, path := range []string{"/v1/chat/completions", "/anthropic/v1/messages"} {
		r := httptest.NewRequest(http.MethodOptions, path, nil)
		r.Header.Set("Origin", "https://console.example")
		w := httptest.NewRecorder()
		s.preflight(w, r)
		exposed := map[string]bool{}
		for name := range strings.SplitSeq(w.Header().Get("Access-Control-Expose-Headers"), ",") {
			exposed[strings.ToLower(strings.TrimSpace(name))] = true
		}
		names := []string{"X-Request-Id", "Retry-After", "X-Should-Retry", "X-OLP-Delivery-Replay",
			headerAttempts, headerRouteRevision, headerProvider, headerCost}
		for _, family := range []*rateHeaders{&openAIRateHeaders, &anthropicRateHeaders} {
			names = append(names, family.limitRequests, family.remainingRequests, family.resetRequests,
				family.limitTokens, family.remainingTokens, family.resetTokens)
		}
		for _, name := range names {
			if !exposed[strings.ToLower(name)] {
				t.Errorf("%s: %s is not exposed in %q", path, name, w.Header().Get("Access-Control-Expose-Headers"))
			}
		}
		if len(exposed) != len(names) {
			t.Errorf("%s: exposes %d headers, want the %d that are written: %q", path, len(exposed), len(names), w.Header().Get("Access-Control-Expose-Headers"))
		}
	}
}

func decimalText(value string) *string { return &value }

func billedFact(price *usage.RoutingPrice, input, output int64) AttemptFact {
	return AttemptFact{
		Class: classSuccess, Price: price, UsageObserved: true, UsageComplete: true,
		Usage: &openai.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output},
	}
}

// failedAfterDispatch is an attempt the upstream may have billed, without saying.
func failedAfterDispatch(price *usage.RoutingPrice) AttemptFact {
	return AttemptFact{Class: classUpstreamServer, Price: price, BillingUncertain: true}
}

// TestResponseCostIsStatedExactlyWhenAccountingWouldPriceTheRequest holds the
// header to the rule that decides a request's cost in the accounting tables: the
// cost of the attempts that reported usage, and none when any attempt that was or
// may have been billed could not be priced.
func TestResponseCostIsStatedExactlyWhenAccountingWouldPriceTheRequest(t *testing.T) {
	// A million tokens of input cost one unit and of output ten.
	price := &usage.RoutingPrice{Price: usage.Price{InputPerMillion: decimalText("1"), OutputPerMillion: decimalText("10")}}
	free := &usage.RoutingPrice{Price: usage.Price{InputPerMillion: decimalText("0"), OutputPerMillion: decimalText("0")}}
	inputOnly := &usage.RoutingPrice{Price: usage.Price{InputPerMillion: decimalText("1")}}
	rejected := AttemptFact{Class: classRateLimit, UsageComplete: true}
	for _, tc := range []struct {
		name   string
		facts  []AttemptFact
		want   string
		priced bool
	}{
		{"one priced attempt", []AttemptFact{billedFact(price, 2, 3)}, "0.000032", true},
		{"a price of nothing is a cost of nothing", []AttemptFact{billedFact(free, 2, 3)}, "0", true},
		{"a failover bills the attempt that served it", []AttemptFact{failedAfterDispatch(price), billedFact(price, 2, 3)}, "0.000032", true},
		{"a local quota refusal was never billable", []AttemptFact{rejected, billedFact(price, 2, 3)}, "0.000032", true},
		{"two attempts that reported usage add up", []AttemptFact{billedFact(price, 1, 0), billedFact(price, 2, 3)}, "0.000033", true},
		{"no price list", []AttemptFact{billedFact(nil, 2, 3)}, "", false},
		{"a price without a rate for what was used", []AttemptFact{billedFact(inputOnly, 2, 3)}, "", false},
		{"a success that reported no usage", []AttemptFact{{Class: classSuccess, Price: price, BillingUncertain: true}}, "", false},
		{"an earlier attempt that cannot be priced", []AttemptFact{failedAfterDispatch(nil), billedFact(price, 2, 3)}, "", false},
		{"no attempt", nil, "", false},
		// A call that was never billable, as a lifecycle read of a stored response
		// is not, has no cost to state, which accounting records as no cost at all.
		{"nothing was billable", []AttemptFact{{Class: classSuccess, Price: price, UsageComplete: true}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &execution{family: openai.FamilyChat, facts: tc.facts}
			cost, ok := x.responseCost()
			if ok != tc.priced || ok && cost.String() != tc.want {
				t.Fatalf("responseCost = %s, %v; want %s, %v", cost, ok, tc.want, tc.priced)
			}
		})
	}
}

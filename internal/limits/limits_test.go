package limits

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// reply builds the array shape GLIDE hands back for a Lua table: integers
// arrive as int64 and strings as string.
func reply(values ...any) []any { return values }

// allowance is the five fields a rate reply appends to the six every reservation
// reply begins with.
type allowance struct{ requestLimit, requestRemaining, tokenLimit, tokenRemaining, resetMS int64 }

// headReply builds the reserve_limits reply of a request that did not ask for the
// allowance, and the one a failure has whatever was asked: the six fields every
// reply begins with.
func headReply(status int64, detail string, retry, window, expiry int64) []any {
	return reply(rateReplyVersion, status, detail, retry, window, expiry)
}

// rateReply builds the reserve_limits reply of a request that asked for the
// allowance: the six fields of headReply and the five that state it.
func rateReply(status int64, detail string, retry, window, expiry int64, a allowance) []any {
	return reply(rateReplyVersion, status, detail, retry, window, expiry,
		a.requestLimit, a.requestRemaining, a.tokenLimit, a.tokenRemaining, a.resetMS)
}

// rateReplyVersion is the reserve_limits reply version, spelled out so that a
// change to the contract has to change these tests.
const rateReplyVersion = int64(2)

// testWindow is a fixed window ID and the instant, in Unix milliseconds, at which
// it ends.
const (
	testWindow      = int64(29_823_060)
	testWindowEndMS = int64(1_789_383_660_000)
)

func TestParseReservationRejectsMalformedResponses(t *testing.T) {
	t.Parallel()
	both := allowance{requestLimit: 10, requestRemaining: 4, tokenLimit: 1000, tokenRemaining: 900, resetMS: 30_000}
	requestsOnly := allowance{requestLimit: 10, requestRemaining: 4, resetMS: 30_000}
	granted := func(mutate func(items []any)) []any {
		items := rateReply(1, "ok", 0, testWindow, 0, both)
		mutate(items)
		return items
	}
	rejected := func(detail string, retry int64, a allowance) []any {
		return rateReply(0, detail, retry, testWindow, 0, a)
	}
	for _, test := range []struct {
		name  string
		value any
	}{
		{"the version before the allowance", []any{int64(1), int64(1), "ok", int64(0), testWindow, int64(0)}},
		{"the version before the allowance with the allowance", granted(func(items []any) { items[0] = int64(1) })},
		{"a later version", granted(func(items []any) { items[0] = int64(3) })},
		{"granted with retry hint", granted(func(items []any) { items[3] = int64(1) })},
		{"granted without a window", granted(func(items []any) { items[4] = int64(0) })},
		{"rejection without retry hint", rejected("rpm", 0, allowance{requestLimit: 10, resetMS: 0})},
		{"retry beyond the window", rejected("rpm", 60_001, allowance{requestLimit: 10, resetMS: 60_001})},
		{"rejection without a window", rateReply(0, "rpm", 1, 0, 0, allowance{requestLimit: 10, resetMS: 1})},
		{"rejection that leased concurrency", rateReply(0, "rpm", 1, testWindow, 1, allowance{requestLimit: 10, resetMS: 1})},
		{"unknown dimension", rejected("unknown", 1, allowance{requestLimit: 10, resetMS: 1})},
		{"unknown status", rateReply(2, "ok", 0, testWindow, 0, both)},
		{"malformed state with a retry hint", headReply(-1, "malformed_rate_state", 1, testWindow, 0)},
		{"malformed state without a window", headReply(-1, "malformed_rate_state", 0, 0, 0)},
		{"malformed state that states an allowance", rateReply(-1, "malformed_rate_state", 0, testWindow, 0, both)},
		{"malformed state that states an empty allowance", rateReply(-1, "malformed_rate_state", 0, testWindow, 0, allowance{})},
		{"script failure with a window", headReply(-1, "invalid_arguments", 0, testWindow, 0)},
		{"script failure that states an allowance", rateReply(-1, "invalid_server_time", 0, 0, 0, allowance{resetMS: 1})},
		{"script failure that states an empty allowance", rateReply(-1, "invalid_arguments", 0, 0, 0, allowance{})},
		{"unknown failure", headReply(-1, "unknown", 0, 0, 0)},
		{"a grant that does not state the allowance it was asked for", headReply(1, "ok", 0, testWindow, 0)},
		{"a rejection that does not state the allowance it was asked for", headReply(0, "rpm", 30_000, testWindow, 0)},
		{"counter beyond the Lua range", granted(func(items []any) { items[4] = maxLuaInteger + 1 })},
		{"negative window", granted(func(items []any) { items[4] = int64(-1) })},
		{"a window that never ends", rateReply(1, "ok", 0, maxLuaInteger/60_000, 0, both)},
		{"remaining beyond the Lua range", granted(func(items []any) { items[7] = maxLuaInteger + 1 })},
		{"negative remaining", granted(func(items []any) { items[7] = int64(-1) })},
		{"negative limit", granted(func(items []any) { items[6] = int64(-1) })},
		{"string where an integer belongs", granted(func(items []any) { items[0] = "2" })},
		{"string where the limit belongs", granted(func(items []any) { items[6] = "10" })},
		{"integer where a string belongs", granted(func(items []any) { items[2] = int64(0) })},
		{"request remaining above its limit", granted(func(items []any) { items[7] = int64(11) })},
		{"token remaining above its limit", granted(func(items []any) { items[9] = int64(1001) })},
		{"request remaining of an unlimited dimension", rateReply(1, "ok", 0, testWindow, 0, allowance{tokenLimit: 1000, requestRemaining: 1, tokenRemaining: 900, resetMS: 30_000})},
		{"token remaining of an unlimited dimension", rateReply(1, "ok", 0, testWindow, 0, allowance{requestLimit: 10, requestRemaining: 4, tokenRemaining: 1, resetMS: 30_000})},
		{"a granted request that left a request limit untouched", granted(func(items []any) { items[7] = int64(10) })},
		{"a granted request that left a token limit untouched", granted(func(items []any) { items[9] = int64(1000) })},
		{"a window that is over", granted(func(items []any) { items[10] = int64(0) })},
		{"a window beyond a minute", granted(func(items []any) { items[10] = int64(60_001) })},
		{"a window of nothing limited", rateReply(1, "ok", 0, testWindow, 0, allowance{resetMS: 30_000})},
		{"requests exhausted with requests left", rejected("rpm", 30_000, both)},
		{"requests exhausted without a request limit", rejected("rpm", 30_000, allowance{tokenLimit: 1000, tokenRemaining: 900, resetMS: 30_000})},
		{"tokens exhausted without a token limit", rejected("tpm", 30_000, requestsOnly)},
		{"requests exhausted with another retry hint", rejected("rpm", 29_000, allowance{requestLimit: 10, resetMS: 30_000})},
		{"tokens exhausted with another retry hint", rejected("tpm", 29_000, both)},
		{"short array", rateReply(1, "ok", 0, testWindow, 0, both)[:10]},
		{"long array", append(rateReply(1, "ok", 0, testWindow, 0, both), int64(0))},
		{"empty array", []any{}},
		{"nil array", []any(nil)},
		{"not an array", int64(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseReservation(test.value, true); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("parseReservation(%v) error = %v, want ErrUnexpectedResponse", test.value, err)
			}
		})
	}
}

// The rate and concurrency keys of one lookup must share a hash tag so a
// single script can touch both, and both cost keys must follow the API key
// no matter which lookup spends from it.

// A counter must never be split per dimension: one hash holds them all.

func TestRequestValidationRejectsBypassableLimits(t *testing.T) {
	t.Parallel()
	const key = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	base := func() Request {
		return Request{
			CostOwnerID:       key,
			LookupID:          "lookup_one_abc",
			RequestsPerMinute: pointer(int64(10)),
			TokensPerMinute:   pointer(int64(1000)),
			MaxConcurrency:    pointer(int64(10)),
			RequestedTokens:   5,
			LeaseTTL:          5 * time.Second,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the base request is invalid: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Request)
	}{
		{"zero requests per minute", func(r *Request) { r.RequestsPerMinute = pointer(int64(0)) }},
		{"negative tokens per minute", func(r *Request) { r.TokensPerMinute = pointer(int64(-1)) }},
		{"negative requested tokens", func(r *Request) { r.RequestedTokens = -1 }},
		{"no tokens requested under a token limit", func(r *Request) { r.RequestedTokens = 0 }},
		{"concurrency beyond the Lua range", func(r *Request) { r.MaxConcurrency = pointer(maxLuaInteger + 1) }},
		{"requested tokens beyond the Lua range", func(r *Request) { r.RequestedTokens = maxLuaInteger + 1 }},
		{"zero lease TTL", func(r *Request) { r.LeaseTTL = 0 }},
		{"sub-millisecond lease TTL", func(r *Request) { r.LeaseTTL = 999 * time.Microsecond }},
		{"lookup with a hash tag", func(r *Request) { r.LookupID = "bad}{slot" }},
		{"lookup too short", func(r *Request) { r.LookupID = "short" }},
		{"lookup too long", func(r *Request) { r.LookupID = strings.Repeat("a", 41) }},
		{"lookup with a dash", func(r *Request) { r.LookupID = "lookup-one-abc" }},
		{"API key that is not a UUID", func(r *Request) { r.CostOwnerID = "not-a-uuid" }},
		{"zero daily cost limit", func(r *Request) { r.DailyCostLimit = pointer("0.000") }},
		{"negative monthly cost limit", func(r *Request) { r.MonthlyCostLimit = pointer("-1") }},
		{"cost limit in scientific notation", func(r *Request) { r.DailyCostLimit = pointer("1e3") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := base()
			test.mutate(&request)
			err := request.Validate()
			var invalid *InvalidRequestError
			if !errors.As(err, &invalid) {
				t.Fatalf("Validate() error = %v, want InvalidRequestError", err)
			}
		})
	}
	// The provider lookups this package builds must pass its own validation.
	for _, lookup := range []string{
		ConnectionLookup("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"),
		SlotLookup("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"),
	} {
		request := base()
		request.LookupID = lookup
		if err := request.Validate(); err != nil {
			t.Fatalf("Validate() rejected the generated lookup %q: %v", lookup, err)
		}
	}
}

// An absent credential version still names a stable scope, and an
// upper-case identifier must address the same key as a lower-case one.

// A leap day whose month also ends that midnight: the day and month windows
// must both close at 2028-03-01, not a day or a month later.

// 2028-02-29 is the 21243rd whole UTC day since the epoch, which is the
// identifier Valkey's own day arithmetic derives from its clock.

// A local-zone instant names the UTC window it falls in, not the local day.

// December must roll the year over rather than name a thirteenth month.

// Without an estimate nothing about the reservation applies.

// The script reads the balances and the reservations together, which a
// cluster allows only under one hash tag, whichever lookup spends.

// A lease without an estimate and a reconcile without a token budget touch
// nothing, so no Valkey connection is needed to finish them.

// A policy that was never loaded must reject rather than admit.

func pointer[T any](value T) *T { return &value }

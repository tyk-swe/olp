package limits

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
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

// TestParseReservationRejectsMalformedResponsesToARequestThatDidNotAsk proves a
// request that did not ask for the allowance is answered with the six fields and
// no more, whatever the decision: a script that answers it with the allowance is
// not the one this package runs, and is refused as one that answered too little
// is.
func TestParseReservationRejectsMalformedResponsesToARequestThatDidNotAsk(t *testing.T) {
	t.Parallel()
	both := allowance{requestLimit: 10, requestRemaining: 4, tokenLimit: 1000, tokenRemaining: 900, resetMS: 30_000}
	for _, test := range []struct {
		name  string
		value any
	}{
		{"a grant that states the allowance", rateReply(1, "ok", 0, testWindow, 0, both)},
		{"a grant that states an empty allowance", rateReply(1, "ok", 0, testWindow, 0, allowance{})},
		{"a rejection that states the allowance", rateReply(0, "rpm", 30_000, testWindow, 0, allowance{10, 0, 1000, 900, 30_000})},
		{"a concurrency rejection that states the allowance", rateReply(0, "concurrency", 4_000, testWindow, 0, both)},
		{"a failure that states an empty allowance", rateReply(-1, "malformed_rate_state", 0, testWindow, 0, allowance{})},
		{"the version before the allowance", []any{int64(1), int64(1), "ok", int64(0), testWindow, int64(0)}},
		{"a later version", reply(int64(3), int64(1), "ok", int64(0), testWindow, int64(0))},
		{"granted with retry hint", headReply(1, "ok", 1, testWindow, 0)},
		{"granted without a window", headReply(1, "ok", 0, 0, 0)},
		{"rejection without retry hint", headReply(0, "rpm", 0, testWindow, 0)},
		{"retry beyond the window", headReply(0, "rpm", 60_001, testWindow, 0)},
		{"token retry beyond the window", headReply(0, "tpm", 60_001, testWindow, 0)},
		{"rejection without a window", headReply(0, "rpm", 1, 0, 0)},
		{"rejection that leased concurrency", headReply(0, "tpm", 1, testWindow, 1)},
		{"unknown dimension", headReply(0, "unknown", 1, testWindow, 0)},
		{"unknown status", headReply(2, "ok", 0, testWindow, 0)},
		{"malformed state with a retry hint", headReply(-1, "malformed_rate_state", 1, testWindow, 0)},
		{"malformed state without a window", headReply(-1, "malformed_rate_state", 0, 0, 0)},
		{"script failure with a window", headReply(-1, "invalid_arguments", 0, testWindow, 0)},
		{"unknown failure", headReply(-1, "unknown", 0, 0, 0)},
		{"counter beyond the Lua range", headReply(1, "ok", 0, maxLuaInteger+1, 0)},
		{"negative window", headReply(1, "ok", 0, -1, 0)},
		{"short array", headReply(1, "ok", 0, testWindow, 0)[:5]},
		{"empty array", []any{}},
		{"nil array", []any(nil)},
		{"not an array", int64(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseReservation(test.value, false); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("parseReservation(%v) error = %v, want ErrUnexpectedResponse", test.value, err)
			}
		})
	}
}

func TestParseReservationAcceptsTheScriptContract(t *testing.T) {
	t.Parallel()
	state := func(requestLimit, requestRemaining, tokenLimit, tokenRemaining, resetMS int64) RateState {
		return RateState{
			RequestLimit: requestLimit, RequestRemaining: requestRemaining,
			TokenLimit: tokenLimit, TokenRemaining: tokenRemaining,
			ResetAfter: time.Duration(resetMS) * time.Millisecond,
			ResetAt:    time.UnixMilli(testWindowEndMS).UTC(),
		}
	}
	for _, test := range []struct {
		name  string
		value any
		want  scriptResult
	}{
		{
			"granted with requests and tokens",
			rateReply(1, "ok", 0, testWindow, 0, allowance{10, 4, 1000, 900, 30_000}),
			scriptResult{kind: resultGranted, windowID: testWindow, rate: state(10, 4, 1000, 900, 30_000)},
		},
		{
			"granted with the last request and the last tokens",
			rateReply(1, "ok", 0, testWindow, 0, allowance{10, 0, 1000, 0, 1}),
			scriptResult{kind: resultGranted, windowID: testWindow, rate: state(10, 0, 1000, 0, 1)},
		},
		{
			"granted with requests only",
			rateReply(1, "ok", 0, testWindow, 0, allowance{requestLimit: 10, requestRemaining: 9, resetMS: 60_000}),
			scriptResult{kind: resultGranted, windowID: testWindow, rate: state(10, 9, 0, 0, 60_000)},
		},
		{
			"granted with tokens only",
			rateReply(1, "ok", 0, testWindow, 0, allowance{tokenLimit: 1000, tokenRemaining: 995, resetMS: 12_345}),
			scriptResult{kind: resultGranted, windowID: testWindow, rate: state(0, 0, 1000, 995, 12_345)},
		},
		{
			"granted with a concurrency lease",
			rateReply(1, "ok", 0, testWindow, 1_789_383_605_000, allowance{10, 4, 0, 0, 30_000}),
			scriptResult{kind: resultGranted, windowID: testWindow, leaseExpiresAtMS: 1_789_383_605_000, rate: state(10, 4, 0, 0, 30_000)},
		},
		{
			"granted with concurrency only",
			rateReply(1, "ok", 0, testWindow, 1_789_383_605_000, allowance{}),
			scriptResult{kind: resultGranted, windowID: testWindow, leaseExpiresAtMS: 1_789_383_605_000},
		},
		{
			"requests exhausted",
			rateReply(0, "rpm", 60_000, testWindow, 0, allowance{10, 0, 1000, 900, 60_000}),
			scriptResult{kind: resultRejected, dimension: DimensionRequests, retryAfterMS: 60_000, rate: state(10, 0, 1000, 900, 60_000)},
		},
		{
			"tokens exhausted",
			rateReply(0, "tpm", 1, testWindow, 0, allowance{10, 3, 1000, 4, 1}),
			scriptResult{kind: resultRejected, dimension: DimensionTokens, retryAfterMS: 1, rate: state(10, 3, 1000, 4, 1)},
		},
		{
			"tokens exhausted beyond a window's capacity",
			rateReply(0, "tpm", 30_000, testWindow, 0, allowance{tokenLimit: 1000, tokenRemaining: 1000, resetMS: 30_000}),
			scriptResult{kind: resultRejected, dimension: DimensionTokens, retryAfterMS: 30_000, rate: state(0, 0, 1000, 1000, 30_000)},
		},
		{
			"concurrency exhausted beyond a minute",
			rateReply(0, "concurrency", 120_000, testWindow, 0, allowance{10, 5, 1000, 995, 30_000}),
			scriptResult{kind: resultRejected, dimension: DimensionConcurrency, retryAfterMS: 120_000, rate: state(10, 5, 1000, 995, 30_000)},
		},
		{
			"concurrency exhausted without a rate limit",
			rateReply(0, "concurrency", 4_000, testWindow, 0, allowance{}),
			scriptResult{kind: resultRejected, dimension: DimensionConcurrency, retryAfterMS: 4_000},
		},
		{
			"malformed rate state",
			headReply(-1, "malformed_rate_state", 0, testWindow, 0),
			scriptResult{kind: resultMalformed},
		},
		{
			"malformed concurrency state",
			headReply(-1, "malformed_concurrency_state", 0, testWindow, 0),
			scriptResult{kind: resultMalformed},
		},
		{
			"invalid arguments",
			headReply(-1, "invalid_arguments", 0, 0, 0),
			scriptResult{kind: resultScriptFailure},
		},
		{
			"invalid server time",
			headReply(-1, "invalid_server_time", 0, 0, 0),
			scriptResult{kind: resultScriptFailure},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseReservation(test.value, true)
			if err != nil {
				t.Fatalf("parseReservation() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseReservation() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestParseReservationAcceptsTheShortContract proves the six fields are the whole
// answer to a request that did not ask for the allowance: the decision is read as
// it is read with the allowance, and states none.
func TestParseReservationAcceptsTheShortContract(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
		want  scriptResult
	}{
		{"granted", headReply(1, "ok", 0, testWindow, 0), scriptResult{kind: resultGranted, windowID: testWindow}},
		{
			"granted with a concurrency lease",
			headReply(1, "ok", 0, testWindow, 1_789_383_605_000),
			scriptResult{kind: resultGranted, windowID: testWindow, leaseExpiresAtMS: 1_789_383_605_000},
		},
		{
			"requests exhausted",
			headReply(0, "rpm", 60_000, testWindow, 0),
			scriptResult{kind: resultRejected, dimension: DimensionRequests, retryAfterMS: 60_000},
		},
		{
			"tokens exhausted",
			headReply(0, "tpm", 1, testWindow, 0),
			scriptResult{kind: resultRejected, dimension: DimensionTokens, retryAfterMS: 1},
		},
		{
			"concurrency exhausted beyond a minute",
			headReply(0, "concurrency", 120_000, testWindow, 0),
			scriptResult{kind: resultRejected, dimension: DimensionConcurrency, retryAfterMS: 120_000},
		},
		{
			"malformed rate state",
			headReply(-1, "malformed_rate_state", 0, testWindow, 0),
			scriptResult{kind: resultMalformed},
		},
		{
			"malformed concurrency state",
			headReply(-1, "malformed_concurrency_state", 0, testWindow, 0),
			scriptResult{kind: resultMalformed},
		},
		{
			"invalid arguments",
			headReply(-1, "invalid_arguments", 0, 0, 0),
			scriptResult{kind: resultScriptFailure},
		},
		{
			"invalid server time",
			headReply(-1, "invalid_server_time", 0, 0, 0),
			scriptResult{kind: resultScriptFailure},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseReservation(test.value, false)
			if err != nil {
				t.Fatalf("parseReservation() error = %v", err)
			}
			if got != test.want || got.rate != (RateState{}) {
				t.Fatalf("parseReservation() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseCostReservationRejectsMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
	}{
		{"unknown version", reply(int64(2), int64(1), "ok", int64(0), int64(20_000), int64(24_000))},
		{"missing day window", reply(int64(1), int64(1), "ok", int64(0), int64(0), int64(24_000))},
		{"missing month window", reply(int64(1), int64(1), "ok", int64(0), int64(20_000), int64(0))},
		{"granted with a retry hint", reply(int64(1), int64(1), "ok", int64(1), int64(20_000), int64(24_000))},
		{"daily retry beyond a day", reply(int64(1), int64(0), "daily_cost", dayMS+1, int64(20_000), int64(24_000))},
		{"monthly retry beyond a month", reply(int64(1), int64(0), "monthly_cost", maxMonthMS+1, int64(20_000), int64(24_000))},
		{"rejection without a retry hint", reply(int64(1), int64(0), "daily_cost", int64(0), int64(20_000), int64(24_000))},
		{"estimate rejection without a retry hint", reply(int64(1), int64(0), "daily_cost_estimate", int64(0), int64(20_000), int64(24_000))},
		{"estimate rejection beyond a day", reply(int64(1), int64(0), "daily_cost_estimate", dayMS+1, int64(20_000), int64(24_000))},
		{"estimate rejection of an unknown window", reply(int64(1), int64(0), "yearly_cost_estimate", int64(1), int64(20_000), int64(24_000))},
		{"uninitialized with a retry hint", reply(int64(1), int64(-1), "uninitialized_daily_cost_state", int64(1), int64(20_000), int64(24_000))},
		{"unknown reason", reply(int64(1), int64(-1), "exploded", int64(0), int64(20_000), int64(24_000))},
		{"short array", reply(int64(1), int64(1), "ok", int64(0), int64(20_000))},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseCostReservation(test.value); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("parseCostReservation(%v) error = %v, want ErrUnexpectedResponse", test.value, err)
			}
		})
	}
}

func TestParseCostReservationAcceptsTheScriptContract(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
		want  scriptResult
	}{
		{
			"granted",
			reply(int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000)),
			scriptResult{kind: resultGranted},
		},
		{
			"daily budget exhausted",
			reply(int64(1), int64(0), "daily_cost", dayMS, int64(20_000), int64(24_000)),
			scriptResult{kind: resultRejected, dimension: DimensionDailyCost, retryAfterMS: dayMS},
		},
		{
			"monthly budget exhausted",
			reply(int64(1), int64(0), "monthly_cost", maxMonthMS, int64(20_000), int64(24_000)),
			scriptResult{kind: resultRejected, dimension: DimensionMonthlyCost, retryAfterMS: maxMonthMS},
		},
		{
			"daily budget cannot hold the estimate",
			reply(int64(1), int64(0), "daily_cost_estimate", int64(1000), int64(20_000), int64(24_000)),
			scriptResult{kind: resultRejected, dimension: DimensionDailyCost, retryAfterMS: 1000, estimate: true},
		},
		{
			"monthly budget cannot hold the estimate",
			reply(int64(1), int64(0), "monthly_cost_estimate", maxMonthMS, int64(20_000), int64(24_000)),
			scriptResult{kind: resultRejected, dimension: DimensionMonthlyCost, retryAfterMS: maxMonthMS, estimate: true},
		},
		{
			"monthly state awaits reconciliation",
			reply(int64(1), int64(-1), "uninitialized_monthly_cost_state", int64(0), int64(20_000), int64(24_000)),
			scriptResult{kind: resultUninitialized},
		},
		{
			"daily state is malformed",
			reply(int64(1), int64(-1), "malformed_daily_cost_state", int64(0), int64(20_000), int64(24_000)),
			scriptResult{kind: resultMalformed},
		},
		{
			"invalid arguments",
			reply(int64(1), int64(-1), "invalid_arguments", int64(0), int64(0), int64(0)),
			scriptResult{kind: resultScriptFailure},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCostReservation(test.value)
			if err != nil {
				t.Fatalf("parseCostReservation() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseCostReservation() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseReconciliation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		value       any
		wantDaily   bool
		wantMonthly bool
		wantErr     error
	}{
		{"nothing reconciled", reply(int64(1), int64(1), "ok", int64(0), int64(0)), false, false, nil},
		{"both windows", reply(int64(1), int64(1), "ok", int64(1), int64(1)), true, true, nil},
		{"only the month", reply(int64(1), int64(1), "ok", int64(0), int64(1)), false, true, nil},
		{"malformed day", reply(int64(1), int64(-1), "malformed_daily_cost_state", int64(0), int64(0)), false, false, ErrMalformedState},
		{"malformed with a count", reply(int64(1), int64(-1), "malformed_daily_cost_state", int64(1), int64(0)), false, false, ErrUnexpectedResponse},
		{"count out of range", reply(int64(1), int64(1), "ok", int64(2), int64(0)), false, false, ErrUnexpectedResponse},
		{"unknown version", reply(int64(2), int64(1), "ok", int64(0), int64(0)), false, false, ErrUnexpectedResponse},
		{"six fields", reply(int64(1), int64(1), "ok", int64(0), int64(0), int64(0)), false, false, ErrUnexpectedResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			daily, monthly, err := parseReconciliation(test.value)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("parseReconciliation() error = %v, want %v", err, test.wantErr)
			}
			if daily != test.wantDaily || monthly != test.wantMonthly {
				t.Fatalf("parseReconciliation() = (%t, %t), want (%t, %t)", daily, monthly, test.wantDaily, test.wantMonthly)
			}
		})
	}
}

// everyBudget is a request bound by a rate limit and a cost budget, which is what
// it takes to be given every key.
func everyBudget(lookup, owner string) Request {
	return Request{
		CostOwnerID: owner, LookupID: lookup,
		RequestsPerMinute: pointer(int64(1)), DailyCostLimit: pointer("1"),
	}
}

func TestKeysShareOneClusterHashTag(t *testing.T) {
	t.Parallel()
	limiter := &Limiter{namespace: "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits"}
	const apiKeyID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	first := limiter.keysFor(everyBudget("lookup_one_abc", apiKeyID))
	second := limiter.keysFor(everyBudget("lookup_two_abc", apiKeyID))

	if got, want := first.rate, "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:{lookup_one_abc}:rate"; got != want {
		t.Fatalf("rate key = %q, want %q", got, want)
	}
	if got, want := first.concurrency, "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:{lookup_one_abc}:concurrency"; got != want {
		t.Fatalf("concurrency key = %q, want %q", got, want)
	}
	if got, want := first.dailyCost, "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:{0192cf87d4ab7f2ea8b1c2d3e4f50607}:cost:day"; got != want {
		t.Fatalf("daily cost key = %q, want %q", got, want)
	}
	if got, want := first.monthlyCost, strings.TrimSuffix(first.dailyCost, "day")+"month"; got != want {
		t.Fatalf("monthly cost key = %q, want %q", got, want)
	}
	// The rate and concurrency keys of one lookup must share a hash tag so a
	// single script can touch both, and both cost keys must follow the API key
	// no matter which lookup spends from it.
	if hashTag(first.rate) != hashTag(first.concurrency) {
		t.Fatalf("rate %q and concurrency %q do not share a hash tag", first.rate, first.concurrency)
	}
	if hashTag(first.rate) == hashTag(second.rate) {
		t.Fatalf("distinct lookups share the hash tag %q", hashTag(first.rate))
	}
	if first.dailyCost != second.dailyCost || first.monthlyCost != second.monthlyCost {
		t.Fatalf("cost keys differ between lookups of one API key: %+v vs %+v", first, second)
	}
	// A counter must never be split per dimension: one hash holds them all.
	if strings.Contains(first.rate, ":rpm:") || strings.Contains(first.rate, ":tpm:") {
		t.Fatalf("rate key %q is split per dimension", first.rate)
	}
	if got, want := limiter.cooldownKey("slot:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"),
		"olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:provider-cooldown:slot:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"; got != want {
		t.Fatalf("cooldown key = %q, want %q", got, want)
	}
}

// hashTag extracts what Valkey Cluster uses to place a key.
func hashTag(key string) string {
	open := strings.IndexByte(key, '{')
	if open < 0 {
		return ""
	}
	closed := strings.IndexByte(key[open:], '}')
	if closed < 0 {
		return ""
	}
	return key[open+1 : open+closed]
}

func TestNamespacesCannotOverrideTheClusterHashTag(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{"typical", "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits", false},
		{"punctuation", "olp-test_ns:limits", false},
		{"empty", "", true},
		{"too long", strings.Repeat("n", 129), true},
		{"opens a hash tag", "olp:{limits}", true},
		{"whitespace", "olp limits", true},
		{"newline", "olp\nlimits", true},
		{"non ascii", "olp:limité", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			limiter, err := New(&coordination.Client{}, test.namespace)
			if test.wantErr {
				if err == nil {
					t.Fatalf("New(%q) succeeded, want an error", test.namespace)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q) error = %v", test.namespace, err)
			}
			if limiter == nil {
				t.Fatal("New() returned no limiter")
			}
		})
	}
	if _, err := New(nil, "limits"); err == nil {
		t.Fatal("New(nil) succeeded, want an error")
	}
}

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

func TestRequestReportsWhichBudgetsApply(t *testing.T) {
	t.Parallel()
	var none Request
	if none.HasHardLimits() || none.HasCostBudget() {
		t.Fatal("an empty request reports budgets it does not have")
	}
	concurrency := Request{MaxConcurrency: pointer(int64(1))}
	if !concurrency.HasHardLimits() || concurrency.HasCostBudget() {
		t.Fatal("a concurrency limit was not reported as a hard limit only")
	}
	monthly := Request{MonthlyCostLimit: pointer("10")}
	if !monthly.HasCostBudget() || !monthly.HasHardLimits() {
		t.Fatal("a cost budget must also count as a hard limit")
	}
}

func TestLookupsAndCooldownScopes(t *testing.T) {
	t.Parallel()
	const provider = "0192CF87-D4AB-7F2E-A8B1-C2D3E4F50607"
	const credential = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	if got, want := ConnectionLookup(provider), "pc_0192cf87d4ab7f2ea8b1c2d3e4f50607"; got != want {
		t.Fatalf("ConnectionLookup() = %q, want %q", got, want)
	}
	if got, want := SlotLookup(provider), "ps_0192cf87d4ab7f2ea8b1c2d3e4f50607"; got != want {
		t.Fatalf("SlotLookup() = %q, want %q", got, want)
	}
	// An absent credential version still names a stable scope, and an
	// upper-case identifier must address the same key as a lower-case one.
	if got, want := CredentialScope(provider, nil, 0),
		"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607:00000000-0000-0000-0000-000000000000"; got != want {
		t.Fatalf("CredentialScope() = %q, want %q", got, want)
	}
	if got, want := CredentialScope(provider, pointer(credential), 0),
		"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"; got != want {
		t.Fatalf("CredentialScope() = %q, want %q", got, want)
	}
	if got, want := SlotScope(provider), "slot:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"; got != want {
		t.Fatalf("SlotScope() = %q, want %q", got, want)
	}
}

func TestBudgetWindowsEndOnFixedUTCBoundaries(t *testing.T) {
	t.Parallel()
	// A leap day whose month also ends that midnight: the day and month windows
	// must both close at 2028-03-01, not a day or a month later.
	windows := BudgetWindows(time.Date(2028, time.February, 29, 23, 59, 59, 999_999_999, time.UTC))
	march := time.Date(2028, time.March, 1, 0, 0, 0, 0, time.UTC)
	if !windows.DailyEnd.Equal(march) || !windows.MonthlyEnd.Equal(march) {
		t.Fatalf("windows end at %v and %v, want both at %v", windows.DailyEnd, windows.MonthlyEnd, march)
	}
	if !windows.DailyStart.Equal(time.Date(2028, time.February, 29, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily window starts at %v", windows.DailyStart)
	}
	if !windows.MonthlyStart.Equal(time.Date(2028, time.February, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly window starts at %v", windows.MonthlyStart)
	}
	if got, want := windows.MonthlyID, int64(2028*12+1); got != want {
		t.Fatalf("monthly window ID = %d, want %d", got, want)
	}
	// 2028-02-29 is the 21243rd whole UTC day since the epoch, which is the
	// identifier Valkey's own day arithmetic derives from its clock.
	if got, want := windows.DailyID, int64(21_243); got != want {
		t.Fatalf("daily window ID = %d, want %d", got, want)
	}
	// A local-zone instant names the UTC window it falls in, not the local day.
	newYork := time.FixedZone("EST", -5*3600)
	local := BudgetWindows(time.Date(2028, time.February, 29, 20, 0, 0, 0, newYork))
	if local.DailyID != BudgetWindows(time.Date(2028, time.March, 1, 1, 0, 0, 0, time.UTC)).DailyID {
		t.Fatalf("a local instant resolved to daily window %d", local.DailyID)
	}
	if local.MonthlyID != int64(2028*12+2) {
		t.Fatalf("a local instant resolved to monthly window %d", local.MonthlyID)
	}
	// December must roll the year over rather than name a thirteenth month.
	december := BudgetWindows(time.Date(2026, time.December, 31, 12, 0, 0, 0, time.UTC))
	if !december.MonthlyEnd.Equal(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("December's month ends at %v", december.MonthlyEnd)
	}
	if got, want := december.MonthlyID, int64(2026*12+11); got != want {
		t.Fatalf("December's monthly window ID = %d, want %d", got, want)
	}
}

func TestDecimalValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value   string
		limit   bool
		accrued bool
	}{
		{"1", true, true},
		{"0", false, true},
		{"0.000000000000", false, true},
		{"10.5", true, true},
		{"999999999999.999999999999", true, true},
		{"1000000000000", false, true},
		{"0.0000000000001", false, false},
		{"", false, false},
		{".5", false, false},
		{"5.", false, false},
		{"-1", false, false},
		{"+1", false, false},
		{"1e3", false, false},
		{" 1", false, false},
		{"1 ", false, false},
		{"1.2.3", false, false},
		{"NaN", false, false},
		{strings.Repeat("9", 97), false, false},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			if got := ValidCostLimit(test.value); got != test.limit {
				t.Fatalf("ValidCostLimit(%q) = %t, want %t", test.value, got, test.limit)
			}
			if got := validDecimal(test.value); got != test.accrued {
				t.Fatalf("validDecimal(%q) = %t, want %t", test.value, got, test.accrued)
			}
		})
	}
}

func TestCostSnapshotValidation(t *testing.T) {
	t.Parallel()
	base := CostSnapshot{
		CostOwnerID:      "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607",
		DailyWindowID:    20_000,
		DailyAccrued:     "1.500000000000",
		MonthlyWindowID:  24_000,
		MonthlyAccrued:   "12.000000000000",
		UnpricedAttempts: 3,
	}
	if err := base.validate(); err != nil {
		t.Fatalf("the base snapshot is invalid: %v", err)
	}
	named := base
	named.RequestID = "0192CF87-D4AB-7F2E-A8B1-C2D3E4F50608"
	if err := named.validate(); err != nil {
		t.Fatalf("a snapshot that names its request is invalid: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*CostSnapshot)
	}{
		{"key is not a UUID", func(s *CostSnapshot) { s.CostOwnerID = "key" }},
		{"negative daily window", func(s *CostSnapshot) { s.DailyWindowID = -1 }},
		{"monthly window beyond the Lua range", func(s *CostSnapshot) { s.MonthlyWindowID = maxLuaInteger + 1 }},
		{"daily accrued is not a decimal", func(s *CostSnapshot) { s.DailyAccrued = "1,5" }},
		{"monthly accrued is negative", func(s *CostSnapshot) { s.MonthlyAccrued = "-0.5" }},
		{"unpriced attempts are negative", func(s *CostSnapshot) { s.UnpricedAttempts = -1 }},
		{"unpriced attempts beyond the Lua range", func(s *CostSnapshot) { s.UnpricedAttempts = maxLuaInteger + 1 }},
		{"request is not a UUID", func(s *CostSnapshot) { s.RequestID = "request" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := base
			test.mutate(&snapshot)
			if err := snapshot.validate(); err == nil {
				t.Fatal("validate() accepted a snapshot Valkey cannot store exactly")
			}
		})
	}
}

func TestParseSettlement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		value   any
		want    bool
		wantErr bool
	}{
		{"settled", reply(int64(1), int64(1), "ok", int64(1), int64(0)), true, false},
		{"nothing left to settle", reply(int64(1), int64(1), "ok", int64(0), int64(0)), false, false},
		{"unknown version", reply(int64(2), int64(1), "ok", int64(1), int64(0)), false, true},
		{"invalid arguments", reply(int64(1), int64(-1), "invalid_arguments", int64(0), int64(0)), false, true},
		{"count out of range", reply(int64(1), int64(1), "ok", int64(2), int64(0)), false, true},
		{"spare field in use", reply(int64(1), int64(1), "ok", int64(1), int64(1)), false, true},
		{"unknown detail", reply(int64(1), int64(1), "done", int64(1), int64(0)), false, true},
		{"six fields", reply(int64(1), int64(1), "ok", int64(1), int64(0), int64(0)), false, true},
		{"not an array", int64(1), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseSettlement(test.value)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("parseSettlement() = (%t, %v), want (%t, error %t)", got, err, test.want, test.wantErr)
			}
			if test.wantErr && !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("parseSettlement() error = %v, want ErrUnexpectedResponse", err)
			}
		})
	}
}

// TestReplyVersionsAdvanceIndependently proves a script family's reply is judged
// against its own version, so changing what one family answers cannot make the
// other family's scripts look foreign.
func TestReplyVersionsAdvanceIndependently(t *testing.T) {
	t.Parallel()
	value := reply(int64(2), int64(1), "ok", int64(0), int64(1), int64(0))
	if _, _, _, _, _, ok := tuple(value, 2); !ok {
		t.Fatal("a reply at the requested version was refused")
	}
	if _, _, _, _, _, ok := tuple(value, 1); ok {
		t.Fatal("a reply at another version was accepted")
	}
}

// TestScriptFamiliesRefuseEachOthersReplies proves the rate and cost parsers each
// refuse a reply made for the other script, now that the two answer in different
// shapes.
func TestScriptFamiliesRefuseEachOthersReplies(t *testing.T) {
	t.Parallel()
	rate := rateReply(1, "ok", 0, testWindow, 0, allowance{requestLimit: 10, requestRemaining: 4, resetMS: 1})
	cost := reply(int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000))
	if _, err := parseReservation(rate, true); err != nil {
		t.Fatalf("parseReservation(rate reply) error = %v", err)
	}
	if _, err := parseCostReservation(cost); err != nil {
		t.Fatalf("parseCostReservation(cost reply) error = %v", err)
	}
	for _, stated := range []bool{true, false} {
		if _, err := parseReservation(cost, stated); !errors.Is(err, ErrUnexpectedResponse) {
			t.Fatalf("parseReservation(cost reply, %t) error = %v, want ErrUnexpectedResponse", stated, err)
		}
	}
	if _, err := parseCostReservation(rate); !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("parseCostReservation(rate reply) error = %v, want ErrUnexpectedResponse", err)
	}
}

// TestCostPendingHelpersAreIdentical holds the three scripts that share the
// reservation helpers to one copy of them. The scripts are run by Valkey one at
// a time and cannot import each other, so each carries the block; a drifted copy
// would settle or release reservations by rules the other scripts do not share.
func TestCostPendingHelpersAreIdentical(t *testing.T) {
	t.Parallel()
	block := func(name, script string) string {
		begin := strings.Index(script, "-- BEGIN cost_pending")
		end := strings.Index(script, "-- END cost_pending")
		if begin < 0 || end < begin {
			t.Fatalf("%s carries no cost_pending block", name)
		}
		return script[begin : end+len("-- END cost_pending")]
	}
	reserve := block("reserve_cost.lua", reserveCostSource)
	for name, source := range map[string]string{
		"settle_cost.lua": settleCostSource, "reconcile_cost.lua": reconcileCostSource,
	} {
		if got := block(name, source); got != reserve {
			t.Fatalf("%s carries a different cost_pending block than reserve_cost.lua", name)
		}
	}
}

func TestCostReservationRequestValidation(t *testing.T) {
	t.Parallel()
	const request = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	base := func() Request {
		return Request{
			CostOwnerID:    "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607",
			LookupID:       "lookup_one_abc",
			DailyCostLimit: pointer("10"),
			LeaseTTL:       5 * time.Second,
			CostEstimate:   "0.25",
			RequestID:      request,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the base request is invalid: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Request)
	}{
		{"estimate without a cost budget", func(r *Request) { r.DailyCostLimit = nil }},
		{"estimate that is zero", func(r *Request) { r.CostEstimate = "0.000" }},
		{"estimate that is negative", func(r *Request) { r.CostEstimate = "-1" }},
		{"estimate in scientific notation", func(r *Request) { r.CostEstimate = "1e3" }},
		{"estimate with too many fractional digits", func(r *Request) { r.CostEstimate = "0.0000000000001" }},
		{"estimate with too many integer digits", func(r *Request) { r.CostEstimate = "1000000000000" }},
		{"estimate without a request", func(r *Request) { r.RequestID = "" }},
		{"estimate under a request that is not a UUID", func(r *Request) { r.RequestID = "request" }},
		{"negative grace", func(r *Request) { r.CostGrace = -time.Second }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := base()
			test.mutate(&request)
			var invalid *InvalidRequestError
			if err := request.Validate(); !errors.As(err, &invalid) {
				t.Fatalf("Validate() error = %v, want InvalidRequestError", err)
			}
		})
	}
	// Without an estimate nothing about the reservation applies.
	unpriced := base()
	unpriced.CostEstimate, unpriced.RequestID = "", ""
	if err := unpriced.Validate(); err != nil {
		t.Fatalf("a request without an estimate is invalid: %v", err)
	}
	if grace := unpriced.costGrace(); grace != DefaultCostGrace {
		t.Fatalf("default grace = %v, want %v", grace, DefaultCostGrace)
	}
	unpriced.CostGrace = time.Second
	if grace := unpriced.costGrace(); grace != time.Second {
		t.Fatalf("grace = %v, want one second", grace)
	}
}

func TestRequestReportsRateLimits(t *testing.T) {
	t.Parallel()
	var none Request
	if none.HasRateLimits() {
		t.Fatal("an empty request reports rate limits it does not have")
	}
	if (Request{DailyCostLimit: pointer("1")}).HasRateLimits() {
		t.Fatal("a cost budget was reported as a rate limit")
	}
	for name, request := range map[string]Request{
		"requests":    {RequestsPerMinute: pointer(int64(1))},
		"tokens":      {TokensPerMinute: pointer(int64(1))},
		"concurrency": {MaxConcurrency: pointer(int64(1))},
	} {
		if !request.HasRateLimits() {
			t.Fatalf("a %s limit was not reported as a rate limit", name)
		}
	}
}

func TestPendingKeysShareTheBalancesHashTag(t *testing.T) {
	t.Parallel()
	limiter := &Limiter{namespace: "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits"}
	const owner = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	first := limiter.keysFor(everyBudget("lookup_one_abc", owner))
	second := limiter.keysFor(everyBudget("lookup_two_abc", owner))
	if got, want := first.pending, "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:{0192cf87d4ab7f2ea8b1c2d3e4f50608}:cost:pending"; got != want {
		t.Fatalf("pending key = %q, want %q", got, want)
	}
	if got, want := first.expiry, strings.TrimSuffix(first.pending, "pending")+"expiry"; got != want {
		t.Fatalf("expiry key = %q, want %q", got, want)
	}
	// The script reads the balances and the reservations together, which a
	// cluster allows only under one hash tag, whichever lookup spends.
	for _, key := range []string{first.pending, first.expiry, first.monthlyCost} {
		if hashTag(key) != hashTag(first.dailyCost) {
			t.Fatalf("%q does not share the hash tag of %q", key, first.dailyCost)
		}
	}
	if first.pending != second.pending || first.expiry != second.expiry {
		t.Fatal("pending keys differ between lookups of one owner")
	}
}

// TestLeasesForwardToTheAttachedGroupLease covers what needs no Valkey: a
// request admitted against a budget group finishes both its leases through the
// one handle it keeps, and a request that holds no lease finishes nothing.
func TestLeasesForwardToTheAttachedGroupLease(t *testing.T) {
	t.Parallel()
	var none *Lease
	none.Attach(&Lease{})
	none.SetActualCost("1")
	if none.HasCostReservation() {
		t.Fatal("no lease reports a cost reservation")
	}
	for name, err := range map[string]error{
		"refund": none.Refund(t.Context()), "reconcile": none.Reconcile(t.Context(), 1),
		"release": none.Release(t.Context()), "settle": none.SettleCost(t.Context()),
	} {
		if err != nil {
			t.Fatalf("a missing lease failed to %s: %v", name, err)
		}
	}

	group := &Lease{costReserved: true}
	key := &Lease{}
	if key.HasCostReservation() {
		t.Fatal("a lease with no estimate reports one")
	}
	key.Attach(nil)
	key.Attach(group)
	if !key.HasCostReservation() {
		t.Fatal("the attached group's estimate is not reported")
	}
	key.SetActualCost("0.5")
	if group.actualCost != "0.5" || key.actualCost != "0.5" {
		t.Fatalf("actual cost = %q / %q, want both leases told", key.actualCost, group.actualCost)
	}
	// A lease without an estimate and a reconcile without a token budget touch
	// nothing, so no Valkey connection is needed to finish them.
	plain := &Lease{}
	plain.Attach(&Lease{})
	if err := errors.Join(plain.Reconcile(t.Context(), 5), plain.Release(t.Context()),
		plain.Refund(t.Context()), plain.SettleCost(t.Context())); err != nil {
		t.Fatalf("finishing leases that hold nothing failed: %v", err)
	}
}

func TestOutcomeAndPolicyNames(t *testing.T) {
	t.Parallel()
	for outcome, want := range map[Outcome]string{
		OutcomeSuccess: "success",
		OutcomeFailure: "failure",
		OutcomeSkipped: "skipped",
		Outcome(9):     "unknown",
	} {
		if got := outcome.String(); got != want {
			t.Fatalf("Outcome(%d).String() = %q, want %q", int(outcome), got, want)
		}
	}
	if got, want := FailOpen.String(), "fail_open"; got != want {
		t.Fatalf("FailOpen.String() = %q, want %q", got, want)
	}
	if got, want := FailClosed.String(), "fail_closed"; got != want {
		t.Fatalf("FailClosed.String() = %q, want %q", got, want)
	}
	// A policy that was never loaded must reject rather than admit.
	var zero OutagePolicy
	if zero != FailClosed {
		t.Fatal("the zero outage policy is not fail-closed")
	}
}

func pointer[T any](value T) *T { return &value }

func TestUntrustedUsageCannotRefundTokenReservation(t *testing.T) {
	for _, observed := range []int64{0, 1, 99, 100, 150} {
		client := &scripted{answer: func(args []string) (any, error) {
			want := "0"
			if observed == 150 {
				want = "50"
			}
			if got := args[len(args)-2]; got != want {
				t.Fatalf("usage %d adjustment %s, want %s", observed, got, want)
			}
			return int64(1), nil
		}}
		limiter, err := New(client, "test")
		if err != nil {
			t.Fatal(err)
		}
		lease := &Lease{limiter: limiter, hasToken: true, reservedTokens: 100}
		if err := lease.Reconcile(t.Context(), observed); err != nil {
			t.Fatal(err)
		}
	}
}

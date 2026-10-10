//go:build integration

package integration_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

// limStatedRequest is limRequest as a key's reservation makes it: one that asks to
// be told what is left of its allowance.
func limStatedRequest(lookup string) limits.Request {
	request := limRequest(lookup)
	request.ReportRate = true
	return request
}

// limWantAllowance checks the counts a state reports.
func limWantAllowance(t *testing.T, state limits.RateState, requestLimit, requestRemaining, tokenLimit, tokenRemaining int64) {
	t.Helper()
	if state.RequestLimit != requestLimit || state.RequestRemaining != requestRemaining ||
		state.TokenLimit != tokenLimit || state.TokenRemaining != tokenRemaining {
		t.Fatalf("allowance = requests %d/%d, tokens %d/%d, want requests %d/%d, tokens %d/%d",
			state.RequestRemaining, state.RequestLimit, state.TokenRemaining, state.TokenLimit,
			requestRemaining, requestLimit, tokenRemaining, tokenLimit)
	}
	if state.LimitsRequests() != (requestLimit > 0) || state.LimitsTokens() != (tokenLimit > 0) ||
		state.Limited() != (requestLimit > 0 || tokenLimit > 0) {
		t.Fatalf("state %+v reports the wrong dimensions as limited", state)
	}
}

// limWantReset checks that a state's reset is the end of the fixed UTC minute
// the call ran in, as Valkey's own clock measured it between two readings the
// test took around the call.
func limWantReset(t *testing.T, state limits.RateState, beforeMS, afterMS int64) {
	t.Helper()
	if beforeMS/60_000 != afterMS/60_000 {
		t.Fatalf("the call spanned a minute boundary: %d to %d", beforeMS, afterMS)
	}
	longest := 60_000 - beforeMS%60_000
	shortest := 60_000 - afterMS%60_000
	if got := state.ResetAfter.Milliseconds(); got < shortest || got > longest {
		t.Fatalf("ResetAfter = %dms, want it inside [%dms,%dms]", got, shortest, longest)
	}
	if end := (beforeMS/60_000 + 1) * 60_000; !state.ResetAt.Equal(time.UnixMilli(end)) || state.ResetAt.Location() != time.UTC {
		t.Fatalf("ResetAt = %v, want %v in UTC", state.ResetAt, time.UnixMilli(end).UTC())
	}
}

// limReserveStated reserves request and reports the state its lease carries, with
// the readings of Valkey's clock around the call.
func limReserveStated(t *testing.T, c *coordination.Client, limiter *limits.Limiter, request limits.Request) (*limits.Lease, limits.RateState, [2]int64) {
	t.Helper()
	before := limServerTimeMS(t, c)
	lease := limReserve(t, limiter, request)
	after := limServerTimeMS(t, c)
	return lease, lease.RateState(), [2]int64{before, after}
}

// limRefusedState reserves request and reports the allowance its refusal carries,
// with the readings of Valkey's clock around the call.
func limRefusedState(t *testing.T, c *coordination.Client, limiter *limits.Limiter, request limits.Request, dimension limits.Dimension) (limits.RateState, [2]int64) {
	t.Helper()
	before := limServerTimeMS(t, c)
	_, err := limiter.Reserve(t.Context(), request)
	after := limServerTimeMS(t, c)
	exceeded, ok := errors.AsType[*limits.ExceededError](err)
	if !ok || exceeded.Dimension != dimension {
		t.Fatalf("Reserve error = %v, want a %s rejection", err, dimension)
	}
	return exceeded.Rate, [2]int64{before, after}
}

func TestLimitsRateStateCountsDownThroughGrantsAndRejections(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-state")
	limiter := limLimiter(t, c, namespace)

	t.Run("requests", func(t *testing.T) {
		lookup := limLookup()
		rateKey, _ := limRateKeys(namespace, lookup)
		limSettleInMinute(t, c, 4*time.Second)
		request := limStatedRequest(lookup)
		request.RequestsPerMinute = limPointer(int64(3))
		request.TokensPerMinute = limPointer(int64(100))
		request.MaxConcurrency = nil
		request.RequestedTokens = 30

		for granted, want := range [...][2]int64{{2, 70}, {1, 40}, {0, 10}} {
			_, state, bracket := limReserveStated(t, c, limiter, request)
			limWantAllowance(t, state, 3, want[0], 100, want[1])
			limWantReset(t, state, bracket[0], bracket[1])
			limWantCounters(t, c, rateKey, int64(granted+1), int64(granted+1)*30)
		}

		// The refusal states what the window holds: the request reserved nothing,
		// so the tokens it asked for are still there.
		state, bracket := limRefusedState(t, c, limiter, request, limits.DimensionRequests)
		limWantAllowance(t, state, 3, 0, 100, 10)
		limWantReset(t, state, bracket[0], bracket[1])
		limWantCounters(t, c, rateKey, 3, 90)
	})

	t.Run("tokens", func(t *testing.T) {
		lookup := limLookup()
		rateKey, _ := limRateKeys(namespace, lookup)
		limSettleInMinute(t, c, 4*time.Second)
		request := limStatedRequest(lookup)
		request.RequestsPerMinute = limPointer(int64(10))
		request.TokensPerMinute = limPointer(int64(100))
		request.MaxConcurrency = nil
		request.RequestedTokens = 60

		_, state, bracket := limReserveStated(t, c, limiter, request)
		limWantAllowance(t, state, 10, 9, 100, 40)
		limWantReset(t, state, bracket[0], bracket[1])

		// Sixty tokens do not fit in the forty that remain, and the refused request
		// did not use up a request of the nine.
		state, bracket = limRefusedState(t, c, limiter, request, limits.DimensionTokens)
		limWantAllowance(t, state, 10, 9, 100, 40)
		limWantReset(t, state, bracket[0], bracket[1])
		limWantCounters(t, c, rateKey, 1, 60)
	})

	t.Run("concurrency", func(t *testing.T) {
		lookup := limLookup()
		rateKey, _ := limRateKeys(namespace, lookup)
		limSettleInMinute(t, c, 4*time.Second)
		request := limStatedRequest(lookup)
		request.MaxConcurrency = limPointer(int64(1))
		lease, state, _ := limReserveStated(t, c, limiter, request)
		limWantAllowance(t, state, 10, 9, 1000, 995)

		// The rate limits admitted the request and the concurrency limit refused it,
		// so the allowance is the one it would have been measured against.
		state, bracket := limRefusedState(t, c, limiter, request, limits.DimensionConcurrency)
		limWantAllowance(t, state, 10, 9, 1000, 995)
		limWantReset(t, state, bracket[0], bracket[1])
		limWantCounters(t, c, rateKey, 1, 5)
		limReleaseTwice(t, lease)
	})
}

func TestLimitsRateStateStatesOnlyTheDimensionsTheKeyLimits(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-state-dimensions")
	limiter := limLimiter(t, c, namespace)

	for _, test := range []struct {
		name  string
		shape func(*limits.Request)
		want  [4]int64
		// counters is what the hash holds afterwards: a dimension that is not
		// limited is never counted.
		counters [2]int64
	}{
		{
			"requests only",
			func(r *limits.Request) { r.TokensPerMinute, r.RequestedTokens = nil, 0 },
			[4]int64{10, 9, 0, 0}, [2]int64{1, 0},
		},
		{
			"tokens only",
			func(r *limits.Request) { r.RequestsPerMinute = nil },
			[4]int64{0, 0, 1000, 995}, [2]int64{0, 5},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := limLookup()
			rateKey, _ := limRateKeys(namespace, lookup)
			limSettleInMinute(t, c, 4*time.Second)
			request := limStatedRequest(lookup)
			request.MaxConcurrency = nil
			test.shape(&request)

			_, state, bracket := limReserveStated(t, c, limiter, request)
			limWantAllowance(t, state, test.want[0], test.want[1], test.want[2], test.want[3])
			limWantReset(t, state, bracket[0], bracket[1])
			limWantCounters(t, c, rateKey, test.counters[0], test.counters[1])
		})
	}

	// Neither a request nor a token limit: the lease holds a concurrency slot and
	// the script counts nothing, so there is no allowance to state. The lease is
	// not nil, which is why the state, not the lease, says whether headers apply.
	t.Run("concurrency only", func(t *testing.T) {
		lookup := limLookup()
		rateKey, _ := limRateKeys(namespace, lookup)
		request := limStatedRequest(lookup)
		request.RequestsPerMinute, request.TokensPerMinute, request.RequestedTokens = nil, nil, 0
		request.MaxConcurrency = limPointer(int64(1))

		lease, state, _ := limReserveStated(t, c, limiter, request)
		if lease == nil || state != (limits.RateState{}) || state.Limited() {
			t.Fatalf("lease = %v, state = %+v, want a lease that states no allowance", lease, state)
		}
		refused, _ := limRefusedState(t, c, limiter, request, limits.DimensionConcurrency)
		if refused != (limits.RateState{}) {
			t.Fatalf("refusal states %+v, want none", refused)
		}
		if got := limInt(t, c, "EXISTS", rateKey); got != 0 {
			t.Fatalf("EXISTS rate = %d, want no rate state", got)
		}
		limReleaseTwice(t, lease)
	})
}

func TestLimitsRateStateFollowsRefundsAndReconciliation(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-state-settle")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	limSettleInMinute(t, c, 4*time.Second)

	request := limStatedRequest(lookup)
	request.MaxConcurrency = nil
	request.RequestedTokens = 100
	first, state, _ := limReserveStated(t, c, limiter, request)
	limWantAllowance(t, state, 10, 9, 1000, 900)

	// Lower provider usage cannot refund the locally reserved estimate, and
	// the next request is measured against that floor.
	if err := first.Reconcile(t.Context(), 10); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	request.RequestedTokens = 30
	second, state, _ := limReserveStated(t, c, limiter, request)
	limWantAllowance(t, state, 10, 8, 1000, 870)

	// A request that never dispatched gives both back.
	if err := second.Refund(t.Context()); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	_, state, _ = limReserveStated(t, c, limiter, request)
	limWantAllowance(t, state, 10, 8, 1000, 870)

	// A lease keeps the allowance it was granted: the counts are what the script
	// answered, not a live reading.
	if got := first.RateState(); got.RequestRemaining != 9 || got.TokenRemaining != 900 {
		t.Fatalf("first lease = %+v, want the counts it was granted with", got)
	}
}

func TestLimitsRateStateNeverReportsANegativeAllowance(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-state-overfull")
	limiter := limLimiter(t, c, namespace)

	for _, test := range []struct {
		name      string
		rpm, tpm  int64
		dimension limits.Dimension
		want      [2]int64
	}{
		// A limit lowered below what the window already holds, and a token
		// reconciliation that saturates at the maximum, leave a counter above its
		// limit. What remains is nothing, not the shortfall.
		{"requests above their limit", 15, 500, limits.DimensionRequests, [2]int64{0, 500}},
		{"tokens above their limit", 3, 1500, limits.DimensionTokens, [2]int64{7, 0}},
		{"both above their limit", 15, 1500, limits.DimensionRequests, [2]int64{0, 0}},
		{"requests at their limit", 10, 0, limits.DimensionRequests, [2]int64{0, 1000}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := limLookup()
			rateKey, _ := limRateKeys(namespace, lookup)
			now := limSettleInMinute(t, c, 4*time.Second)
			do(t, c, "HSET", rateKey, "window", strconv.FormatInt(now/60_000, 10),
				"rpm", strconv.FormatInt(test.rpm, 10), "tpm", strconv.FormatInt(test.tpm, 10))
			do(t, c, "PEXPIRE", rateKey, "60000")
			request := limStatedRequest(lookup)
			request.MaxConcurrency = nil

			state, _ := limRefusedState(t, c, limiter, request, test.dimension)
			limWantAllowance(t, state, 10, test.want[0], 1000, test.want[1])
			limWantCounters(t, c, rateKey, test.rpm, test.tpm)
		})
	}
}

func TestLimitsRateStateStartsOverInAnEmptyWindow(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-state-stale")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)

	now := limSettleInMinute(t, c, 4*time.Second)
	// The previous window ran out of both, which says nothing of this one.
	do(t, c, "HSET", rateKey, "window", strconv.FormatInt(now/60_000-1, 10), "rpm", "10", "tpm", "1000")
	do(t, c, "PEXPIRE", rateKey, "120000")
	request := limStatedRequest(lookup)
	request.MaxConcurrency = nil
	request.RequestedTokens = 7

	_, state, bracket := limReserveStated(t, c, limiter, request)
	limWantAllowance(t, state, 10, 9, 1000, 993)
	limWantReset(t, state, bracket[0], bracket[1])
	limWantCounters(t, c, rateKey, 1, 7)
}

// TestLimitsRateReplyShapes proves every answer the script gives has the shape
// the call asked for: the six fields of the decision, and the five of the allowance
// after them when the call asked for it, which is a larger reply than a caller that
// does not read the allowance should be made to receive. A failure answers the six
// whatever was asked, since it can come before the call has been read. A reply that
// was shorter or longer than that would only be found when the rare state that
// produces it occurred.
func TestLimitsRateReplyShapes(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-reply")
	source, err := os.ReadFile("../../internal/limits/scripts/reserve_limits.lua")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)

	type seed func(t *testing.T, rateKey, concurrencyKey string)
	for _, test := range []struct {
		name   string
		seed   seed
		args   []string // rpm, tpm, requested tokens, concurrency
		status int64
		detail string
		// tail is the five fields after the first six of a reply that states the
		// allowance, where limAnyReset stands for whatever the minute had left when
		// the script ran.
		tail [5]int64
	}{
		{"granted", nil, []string{"10", "1000", "5", "2"}, 1, "ok", [5]int64{10, 9, 1000, 995, limAnyReset}},
		{"granted without rate limits", nil, []string{"0", "0", "0", "2"}, 1, "ok", [5]int64{0, 0, 0, 0, 0}},
		{
			"requests exhausted",
			func(t *testing.T, rate, _ string) {
				do(t, c, "HSET", rate, "window", limWindowNow(t, c), "rpm", "10", "tpm", "0")
				do(t, c, "PEXPIRE", rate, "60000")
			},
			[]string{"10", "1000", "5", "0"}, 0, "rpm", [5]int64{10, 0, 1000, 1000, -1},
		},
		{
			"tokens exhausted",
			func(t *testing.T, rate, _ string) {
				do(t, c, "HSET", rate, "window", limWindowNow(t, c), "rpm", "3", "tpm", "999")
				do(t, c, "PEXPIRE", rate, "60000")
			},
			[]string{"10", "1000", "5", "0"}, 0, "tpm", [5]int64{10, 7, 1000, 1, -1},
		},
		{
			"concurrency exhausted",
			func(t *testing.T, _, concurrency string) {
				do(t, c, "ZADD", concurrency, strconv.FormatInt(limServerTimeMS(t, c)+30_000, 10), "held")
				do(t, c, "PEXPIRE", concurrency, "30000")
			},
			[]string{"10", "1000", "5", "1"}, 0, "concurrency", [5]int64{10, 10, 1000, 1000, -1},
		},
		{
			"malformed rate state",
			func(t *testing.T, rate, _ string) { do(t, c, "HSET", rate, "window", "garbage") },
			[]string{"10", "1000", "5", "0"}, -1, "malformed_rate_state", [5]int64{},
		},
		{
			"malformed rate state of a wrong type",
			func(t *testing.T, rate, _ string) { do(t, c, "SET", rate, "garbage") },
			[]string{"10", "1000", "5", "0"}, -1, "malformed_rate_state", [5]int64{},
		},
		{
			"malformed concurrency state",
			func(t *testing.T, _, concurrency string) { do(t, c, "SET", concurrency, "garbage") },
			[]string{"10", "1000", "5", "1"}, -1, "malformed_concurrency_state", [5]int64{},
		},
		{"invalid arguments", nil, []string{"10", "1000", "0", "0"}, -1, "invalid_arguments", [5]int64{}},
	} {
		for _, report := range []string{"1", "0"} {
			t.Run(test.name+" asked "+report, func(t *testing.T) {
				lookup := limLookup()
				rateKey, concurrencyKey := limRateKeys(namespace, lookup)
				limSettleInMinute(t, c, 2*time.Second)
				if test.seed != nil {
					test.seed(t, rateKey, concurrencyKey)
				}
				args := append(append([]string{}, test.args...), "lease", "5000", report)
				before := limServerTimeMS(t, c)
				value := do(t, c, append([]string{"EVAL", script, "2", rateKey, concurrencyKey}, args...)...)
				after := limServerTimeMS(t, c)
				items, ok := value.([]any)
				want := 6
				if report == "1" && test.status != -1 {
					want = 11
				}
				if !ok || len(items) != want {
					t.Fatalf("EVAL = %#v, want %d fields", value, want)
				}
				if items[0] != int64(2) || items[1] != test.status || items[2] != test.detail {
					t.Fatalf("EVAL = %#v, want version 2, status %d, %q", items, test.status, test.detail)
				}
				if want == 6 {
					return
				}
				for index, want := range test.tail {
					got, ok := items[6+index].(int64)
					if !ok {
						t.Fatalf("field %d = %#v, want an integer", 6+index, items[6+index])
					}
					if want == limAnyReset {
						if shortest, longest := 60_000-after%60_000, 60_000-before%60_000; got < shortest || got > longest {
							t.Fatalf("reset = %d, want it inside [%d,%d]", got, shortest, longest)
						}
						continue
					}
					if got != want {
						t.Fatalf("field %d = %d, want %d (reply %#v)", 6+index, got, want, items)
					}
				}
			})
		}
	}
}

// TestLimitsRateScriptRefusesAnArgumentListThatIsNotItsOwn proves the script takes
// the switch that asks for the allowance as a seventh argument that is exactly "0"
// or "1", and refuses before it reads or writes anything a list of any other length:
// the lists of a script that did not know the switch are not its own.
func TestLimitsRateScriptRefusesAnArgumentListThatIsNotItsOwn(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-arguments")
	source, err := os.ReadFile("../../internal/limits/scripts/reserve_limits.lua")
	if err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no switch":              {"10", "1000", "5", "2", "lease", "5000"},
		"two switches":           {"10", "1000", "5", "2", "lease", "5000", "1", "1"},
		"a switch that is two":   {"10", "1000", "5", "2", "lease", "5000", "2"},
		"a switch that is true":  {"10", "1000", "5", "2", "lease", "5000", "true"},
		"a switch that is empty": {"10", "1000", "5", "2", "lease", "5000", ""},
		"a switch with a sign":   {"10", "1000", "5", "2", "lease", "5000", "+1"},
		"a switch with padding":  {"10", "1000", "5", "2", "lease", "5000", "01"},
	} {
		t.Run(name, func(t *testing.T) {
			rateKey, concurrencyKey := limRateKeys(namespace, limLookup())
			value := do(t, c, append([]string{"EVAL", string(source), "2", rateKey, concurrencyKey}, args...)...)
			items, ok := value.([]any)
			if !ok || len(items) != 6 || items[0] != int64(2) || items[1] != int64(-1) || items[2] != "invalid_arguments" {
				t.Fatalf("EVAL = %#v, want an invalid_arguments failure of six fields", value)
			}
			for _, key := range []string{rateKey, concurrencyKey} {
				if got := limInt(t, c, "EXISTS", key); got != 0 {
					t.Fatalf("EXISTS %s = %d, want nothing written", key, got)
				}
			}
		})
	}
}

// limReplyRecorder is a Commander in front of a Valkey client that keeps the
// number of fields in every reply the rate reservation script gives, as the client
// decodes them: what is decoded for each field is what a reply costs the caller.
type limReplyRecorder struct {
	inner limits.Commander
	mu    sync.Mutex
	sizes []int
}

func (r *limReplyRecorder) Do(ctx context.Context, args ...string) (any, error) {
	value, err := r.inner.Do(ctx, args...)
	// The rate reservation is the one script called with a rate key and a
	// concurrency key and seven arguments.
	if err == nil && len(args) == 12 && (args[0] == "EVALSHA" || args[0] == "EVAL") &&
		strings.HasSuffix(args[3], ":rate") && strings.HasSuffix(args[4], ":concurrency") {
		items, _ := value.([]any)
		r.mu.Lock()
		r.sizes = append(r.sizes, len(items))
		r.mu.Unlock()
	}
	return value, err
}

func (r *limReplyRecorder) take() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	sizes := r.sizes
	r.sizes = nil
	return sizes
}

// TestLimitsReplyIsLargerOnlyForAReservationThatReportsTheAllowance proves, on the
// wire, that the reply a reservation is given has the eleven fields only when the
// reservation is a key's with a request or token limit, and the six that decide it
// for every other: a provider's connection or credential quota, which is reserved
// on every attempt and never reports, a key bound by concurrency alone, and any
// state the script cannot read.
func TestLimitsReplyIsLargerOnlyForAReservationThatReportsTheAllowance(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rate-reply-size")
	recorder := &limReplyRecorder{inner: c}
	limiter, err := limits.New(recorder, namespace)
	if err != nil {
		t.Fatal(err)
	}
	// The limit that refuses the second of two reservations is one request, or one
	// slot where nothing limits requests.
	const decision, allowance = 6, 11
	for _, test := range []struct {
		name  string
		shape func(*limits.Request)
		want  int
	}{
		{"a key limited by requests and tokens", func(r *limits.Request) { r.RequestsPerMinute = limPointer(int64(1)); r.ReportRate = true }, allowance},
		{"a key limited by requests", func(r *limits.Request) {
			r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens = limPointer(int64(1)), nil, 0
			r.ReportRate = true
		}, allowance},
		{"a key limited by tokens", func(r *limits.Request) {
			r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens = nil, limPointer(int64(5)), 5
			r.ReportRate = true
		}, allowance},
		{"a key limited by concurrency alone", func(r *limits.Request) {
			r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens, r.MaxConcurrency = nil, nil, 0, limPointer(int64(1))
			r.ReportRate = true
		}, decision},
		{"a provider connection limited by requests and tokens", func(r *limits.Request) { r.RequestsPerMinute = limPointer(int64(1)) }, decision},
		{"a credential slot limited by requests", func(r *limits.Request) {
			r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens = limPointer(int64(1)), nil, 0
		}, decision},
		{"a provider quota limited by concurrency alone", func(r *limits.Request) {
			r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens, r.MaxConcurrency = nil, nil, 0, limPointer(int64(1))
		}, decision},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := limRequest(limLookup())
			request.MaxConcurrency = nil
			test.shape(&request)
			recorder.take()

			lease := limReserve(t, limiter, request)
			_, err := limiter.Reserve(t.Context(), request)
			if _, refused := errors.AsType[*limits.ExceededError](err); !refused {
				t.Fatalf("the second Reserve = %v, want a rejection", err)
			}
			if got := recorder.take(); len(got) != 2 || got[0] != test.want || got[1] != test.want {
				t.Fatalf("reply sizes of a grant and a rejection = %v, want %d for both", got, test.want)
			}
			if got := lease.RateState().Limited(); got != (test.want == allowance) {
				t.Fatalf("RateState = %+v, want it stated exactly when the reply states it", lease.RateState())
			}
			limReleaseTwice(t, lease)
		})
	}

	t.Run("a state the script cannot read", func(t *testing.T) {
		lookup := limLookup()
		rateKey, _ := limRateKeys(namespace, lookup)
		do(t, c, "HSET", rateKey, "window", "garbage")
		request := limStatedRequest(lookup)
		recorder.take()
		if _, err := limiter.Reserve(t.Context(), request); !errors.Is(err, limits.ErrMalformedState) {
			t.Fatalf("Reserve error = %v, want ErrMalformedState", err)
		}
		if got := recorder.take(); len(got) != 1 || got[0] != decision {
			t.Fatalf("reply sizes of a failure that was asked for the allowance = %v, want %d", got, decision)
		}
	})
}

// limAnyReset marks a reset the test only bounds by Valkey's clock.
const limAnyReset = int64(-1)

// limWindowNow is the fixed UTC window Valkey is in, as the script stores it.
func limWindowNow(t *testing.T, c *coordination.Client) string {
	t.Helper()
	return strconv.FormatInt(limServerTimeMS(t, c)/60_000, 10)
}

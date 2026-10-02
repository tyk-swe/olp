package limits

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
)

// scripted is a Commander that answers every command from a function and keeps
// what it was asked, so what a limiter sends can be read without a Valkey.
type scripted struct {
	mu     sync.Mutex
	asked  [][]string
	answer func(args []string) (any, error)
}

func (c *scripted) Do(_ context.Context, args ...string) (any, error) {
	c.mu.Lock()
	c.asked = append(c.asked, slices.Clone(args))
	c.mu.Unlock()
	return c.answer(args)
}

// commands is what the limiter has sent so far.
func (c *scripted) commands() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

// ran counts the scripts that were called with the digest of the script.
func (c *scripted) ran(s script) int {
	count := 0
	for _, args := range c.commands() {
		if len(args) > 1 && (args[0] == "EVALSHA" && args[1] == s.digest || args[0] == "EVAL" && args[1] == s.source) {
			count++
		}
	}
	return count
}

var (
	// noScriptReply is how a server that holds no script with the digest answers,
	// in the words GLIDE gives it and in the server's own.
	noScriptReply = &coordination.CommandError{
		Cause: errors.New("An error was signalled by the server: - NoScriptError: No matching script."),
	}
	rawNoScriptReply = &coordination.CommandError{Cause: errors.New("NOSCRIPT No matching script. Please use EVAL.")}
	// lostReply is a command that timed out: it may have run.
	lostReply = &coordination.CommandError{Cause: context.DeadlineExceeded, Ambiguous: true}
	// refusedReply is a command the server rejected before running anything.
	refusedReply = &coordination.CommandError{Cause: errors.New("connection refused")}

	costGranted  = reply(int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000))
	costSettled  = reply(int64(1), int64(1), "ok", int64(1), int64(0))
	costRefusing = reply(int64(1), int64(0), "daily_cost_estimate", int64(1000), int64(20_000), int64(24_000))
	costUnopened = reply(int64(1), int64(-1), "uninitialized_daily_cost_state", int64(0), int64(20_000), int64(24_000))
)

// The rate replies answer costRequest(true), which limits requests to ten and
// does not ask for the allowance, so they are the decision alone.
var (
	rateGranted = headReply(1, "ok", 0, 5, 0)
	rateRefused = headReply(0, "rpm", 1000, 5, 0)
)

func allScripts() map[string]script {
	return map[string]script{
		"reserve_limits": reserveLimitsScript, "reconcile_limits": reconcileLimitsScript,
		"refund_limits": refundLimitsScript, "release_concurrency": releaseConcurrencyScript,
		"reserve_cost": reserveCostScript, "reconcile_cost": reconcileCostScript,
		"settle_cost": settleCostScript, "provider_usage": providerUsageScript,
		"provider_cooldown": providerCooldownScript,
	}
}

// TestScriptsAreNamedByTheDigestOfTheirSource proves the digest a call names is
// the one Valkey computes for the source: a wrong one would be refused as unknown
// on every call and the source sent each time, which is what naming it avoids.
func TestScriptsAreNamedByTheDigestOfTheirSource(t *testing.T) {
	t.Parallel()
	for name, s := range allScripts() {
		if s.source == "" {
			t.Fatalf("%s embeds no source", name)
		}
		sum := sha1.Sum([]byte(s.source))
		if want := hex.EncodeToString(sum[:]); s.digest != want {
			t.Fatalf("%s digest = %q, want the SHA-1 of its source %q", name, s.digest, want)
		}
	}
}

// TestEveryFileInTheScriptsDirectoryIsAnEmbeddedScript proves nothing sits beside
// the scripts the limiter runs: a stray copy, such as an editor's or a merge's
// leftover, would be a second source of truth for a script whose digest the
// limiter pins, and would drift from it unnoticed.
func TestEveryFileInTheScriptsDirectoryIsAnEmbeddedScript(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("scripts")
	if err != nil {
		t.Fatal(err)
	}
	embedded := allScripts()
	found := map[string]bool{}
	for _, entry := range entries {
		name, isScript := strings.CutSuffix(entry.Name(), ".lua")
		if _, ok := embedded[name]; !ok || !isScript || entry.IsDir() {
			t.Errorf("scripts/%s is not one of the scripts the limiter embeds", entry.Name())
		}
		found[name] = true
	}
	for name := range embedded {
		if !found[name] {
			t.Errorf("the limiter embeds %s but scripts/%s.lua is missing", name, name)
		}
	}
}

func TestEvalSendsTheSourceOnlyToAServerThatHasNone(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{"as the driver says it": noScriptReply, "as the server says it": rawNoScriptReply} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cached := false
			server := &scripted{answer: func(args []string) (any, error) {
				if args[0] == "EVAL" {
					cached = true
					return "first", nil
				}
				if !cached {
					return nil, refusal
				}
				return "later", nil
			}}
			limiter := &Limiter{client: server, namespace: "olp:test"}
			value, err := limiter.eval(t.Context(), reserveCostScript, []string{"k1", "k2"}, "a", "b")
			if err != nil || value != "first" {
				t.Fatalf("eval = %v, %v, want the answer of the call that carried the source", value, err)
			}
			want := [][]string{
				{"EVALSHA", reserveCostScript.digest, "2", "k1", "k2", "a", "b"},
				{"EVAL", reserveCostScript.source, "2", "k1", "k2", "a", "b"},
			}
			if got := server.commands(); !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("commands = %q, want the digest and then the source", got)
			}
			// The server holds the script now, so the next call names it and no more.
			if value, err = limiter.eval(t.Context(), reserveCostScript, []string{"k1", "k2"}, "a", "b"); err != nil || value != "later" {
				t.Fatalf("second eval = %v, %v", value, err)
			}
			got := server.commands()
			if len(got) != 3 || got[2][0] != "EVALSHA" {
				t.Fatalf("commands = %q, want the second call to name the script and not send it", got)
			}
		})
	}
}

// TestEvalNeverRunsAScriptTwice proves only a refusal that came before anything
// ran is sent again: any other failure may have run the script, and a second
// run would reserve twice.
func TestEvalNeverRunsAScriptTwice(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"a script the server does not hold", noScriptReply, 2},
		{"a timeout", lostReply, 1},
		{"a refusal that names no missing script", refusedReply, 1},
		{"a lost reply that mentions a missing script", &coordination.CommandError{Cause: errors.New("NOSCRIPT"), Ambiguous: true}, 1},
		{"a failure that is not a command error", errors.New("NOSCRIPT"), 1},
		{"a command error without a cause", &coordination.CommandError{}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := &scripted{answer: func([]string) (any, error) { return nil, test.err }}
			limiter := &Limiter{client: server, namespace: "olp:test"}
			_, err := limiter.eval(t.Context(), settleCostScript, []string{"k"})
			var service *ServiceError
			if !errors.As(err, &service) {
				t.Fatalf("eval error = %v, want a ServiceError", err)
			}
			if got := len(server.commands()); got != test.want {
				t.Fatalf("%d commands were sent, want %d", got, test.want)
			}
		})
	}
}

// costRequest is a cost-only request with an estimate, and optionally a limit on
// requests that makes it touch the rate script too.
func costRequest(rate bool) Request {
	r := Request{
		CostOwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", LookupID: "lookup_one_abc",
		DailyCostLimit: pointer("10"), LeaseTTL: 5 * time.Second,
		CostEstimate: "0.25", RequestID: "0192CF87-D4AB-7F2E-A8B1-C2D3E4F50608",
	}
	if rate {
		r.RequestsPerMinute = pointer(int64(10))
	}
	return r
}

// answering replies to the script a command names, and to nothing it does not
// know with an error, so that a command the limiter should not send is noticed.
func answering(t *testing.T, replies map[string][2]any) func([]string) (any, error) {
	t.Helper()
	return func(args []string) (any, error) {
		var name string
		for n, s := range allScripts() {
			if args[1] == s.digest || args[1] == s.source {
				name = n
			}
		}
		answer, ok := replies[name]
		if !ok {
			t.Errorf("unexpected command %q", args[:2])
			return nil, refusedReply
		}
		err, _ := answer[1].(error)
		return answer[0], err
	}
}

// TestReserveGivesBackAnEstimateWhoseOutcomeIsUnknown proves a reservation whose
// reply was lost is released again, because the script may have run and nothing
// else will settle it, and that a reservation that cannot have been made is not.
func TestReserveGivesBackAnEstimateWhoseOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		reply    [2]any
		noEstim  bool
		released bool
		wantErr  func(error) bool
	}{
		{"the command timed out", [2]any{nil, lostReply}, false, true, isService},
		{"the reply cannot be read", [2]any{"garbage", nil}, false, true, isUnexpected},
		{"the reply is another script's", [2]any{reply(int64(2), int64(1), "ok", int64(0), int64(20_000), int64(24_000)), nil}, false, true, isUnexpected},
		{"the command was rejected before it ran", [2]any{nil, refusedReply}, false, false, isService},
		{"the budget refused the request", [2]any{costRefusing, nil}, false, false, isExceeded},
		{"the balance awaits reconciliation", [2]any{costUnopened, nil}, false, false, func(err error) bool { return errors.Is(err, ErrUninitializedCost) }},
		{"a request with no estimate cannot have reserved one", [2]any{nil, lostReply}, true, false, isService},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := &scripted{answer: answering(t, map[string][2]any{
				"reserve_cost": test.reply, "settle_cost": {costSettled, nil},
			})}
			limiter := &Limiter{client: server, namespace: "olp:test"}
			request := costRequest(false)
			if test.noEstim {
				request.CostEstimate, request.RequestID = "", ""
			}
			lease, err := limiter.Reserve(t.Context(), request)
			if lease != nil || !test.wantErr(err) {
				t.Fatalf("Reserve = %v, %v", lease, err)
			}
			if released := server.ran(settleCostScript); (released == 1) != test.released || released > 1 {
				t.Fatalf("the reservation was released %d times, want released = %t", released, test.released)
			}
			if !test.released {
				return
			}
			// It is released under the request it was made for, with nothing left to
			// hold, by one bounded attempt and not a series.
			pending, expiry := limiter.pendingKeys(request.CostOwnerID)
			commands := server.commands()
			release := commands[len(commands)-1]
			want := []string{"EVALSHA", settleCostScript.digest, "2", pending, expiry, "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608", "0", "0"}
			if !slices.Equal(release, want) {
				t.Fatalf("release = %q, want %q", release, want)
			}
		})
	}
}

func isService(err error) bool {
	_, ok := errors.AsType[*ServiceError](err)
	return ok
}

func isUnexpected(err error) bool { return errors.Is(err, ErrUnexpectedResponse) }

func isExceeded(err error) bool {
	_, ok := errors.AsType[*ExceededError](err)
	return ok
}

// TestReserveGivesBackTheEstimateWhenTheRateScriptFails proves a request refused
// by its rate limits, or lost to an outage, after its cost was reserved does not
// keep the cost, and that an outage is asked once rather than waited on.
func TestReserveGivesBackTheEstimateWhenTheRateScriptFails(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		rate         [2]any
		settle       [2]any
		wantReleases int
	}{
		{"a rate limit refuses the request", [2]any{rateRefused, nil}, [2]any{costSettled, nil}, 1},
		{"the rate script is lost", [2]any{nil, lostReply}, [2]any{costSettled, nil}, 1},
		{"the outage reaches the release", [2]any{nil, lostReply}, [2]any{nil, refusedReply}, 1},
		{"a release that fails is retried", [2]any{rateRefused, nil}, [2]any{nil, refusedReply}, cleanupAttempts},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := &scripted{answer: answering(t, map[string][2]any{
				"reserve_cost": {costGranted, nil}, "reserve_limits": test.rate, "settle_cost": test.settle,
			})}
			limiter := &Limiter{client: server, namespace: "olp:test"}
			if lease, err := limiter.Reserve(t.Context(), costRequest(true)); lease != nil || err == nil {
				t.Fatalf("Reserve = %v, %v, want a failure", lease, err)
			}
			if got := server.ran(settleCostScript); got != test.wantReleases {
				t.Fatalf("the estimate was released %d times, want %d", got, test.wantReleases)
			}
		})
	}
}

// rateRequest limits the requests and tokens one lookup may use in a minute, and
// asks to be told what is left of them.
func rateRequest() Request {
	return Request{
		CostOwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", LookupID: "lookup_one_abc",
		RequestsPerMinute: pointer(int64(10)), TokensPerMinute: pointer(int64(1000)),
		RequestedTokens: 5, LeaseTTL: 5 * time.Second, ReportRate: true,
	}
}

// reserveAnswered reserves request against a server whose rate script answers with
// answer.
func reserveAnswered(t *testing.T, request Request, answer any) (*Lease, error) {
	t.Helper()
	server := &scripted{answer: answering(t, map[string][2]any{"reserve_limits": {answer, nil}})}
	return (&Limiter{client: server, namespace: "olp:test"}).Reserve(t.Context(), request)
}

// TestReserveAsksForTheAllowanceOnlyWhereItIsReported proves a reservation is
// answered with the allowance only when its caller reads it and a request or token
// limit gives it something to state: the answer is larger, and the quota of a
// provider, or a key that limits neither, is not made to receive it. What the
// script is asked is what the reply is held to, so one that answers more or less
// is refused, whichever way it errs.
func TestReserveAsksForTheAllowanceOnlyWhereItIsReported(t *testing.T) {
	t.Parallel()
	asked := func(r Request, mutate func(*Request)) Request { mutate(&r); return r }
	provider := func(r *Request) { r.ReportRate = false }
	concurrency := func(r *Request) {
		r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens, r.MaxConcurrency = nil, nil, 0, pointer(int64(3))
	}
	requestsOnly := func(r *Request) { r.TokensPerMinute, r.RequestedTokens = nil, 0 }
	tokensOnly := func(r *Request) { r.RequestsPerMinute = nil }
	for _, test := range []struct {
		name    string
		request Request
		stated  bool
	}{
		{"a key with requests and tokens", rateRequest(), true},
		{"a key with requests only", asked(rateRequest(), requestsOnly), true},
		{"a key with tokens only", asked(rateRequest(), tokensOnly), true},
		{"a key with concurrency only", asked(rateRequest(), concurrency), false},
		{"a provider quota with requests and tokens", asked(rateRequest(), provider), false},
		{"a provider quota with requests only", asked(asked(rateRequest(), provider), requestsOnly), false},
		{"a provider quota with concurrency only", asked(asked(rateRequest(), provider), concurrency), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			expiry := int64(0)
			if test.request.MaxConcurrency != nil {
				expiry = 1_789_383_605_000
			}
			full := allowance{
				limitValue(test.request.RequestsPerMinute), limitValue(test.request.RequestsPerMinute) - 1,
				limitValue(test.request.TokensPerMinute), limitValue(test.request.TokensPerMinute) - 5, 30_000,
			}
			if full.requestLimit == 0 {
				full.requestRemaining = 0
			}
			if full.tokenLimit == 0 {
				full.tokenRemaining = 0
			}
			if full.requestLimit == 0 && full.tokenLimit == 0 {
				full.resetMS = 0
			}
			right, wrong := headReply(1, "ok", 0, testWindow, expiry), rateReply(1, "ok", 0, testWindow, expiry, full)
			if test.stated {
				right, wrong = wrong, right
			}

			server := &scripted{answer: answering(t, map[string][2]any{"reserve_limits": {right, nil}})}
			limiter := &Limiter{client: server, namespace: "olp:test"}
			lease, err := limiter.Reserve(t.Context(), test.request)
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			sent := server.commands()
			if len(sent) != 1 || len(sent[0]) != 12 {
				t.Fatalf("commands = %q, want the rate script called once with seven arguments", sent)
			}
			want := "0"
			if test.stated {
				want = "1"
			}
			if got := sent[0][len(sent[0])-1]; got != want {
				t.Fatalf("report_allowance = %q, want %q", got, want)
			}
			if got := lease.RateState().Limited(); got != test.stated {
				t.Fatalf("RateState = %+v, want it stated = %t", lease.RateState(), test.stated)
			}
			if (lease.rate != nil) != test.stated {
				t.Fatalf("a lease holds an allowance = %t, want %t", lease.rate != nil, test.stated)
			}

			// The same request answered the other way is not the script that was run.
			lease, err = reserveAnswered(t, test.request, wrong)
			if lease != nil || !isUnexpected(err) {
				t.Fatalf("Reserve answered the other way = %v, %v, want an unexpected response", lease, err)
			}
		})
	}
}

// TestRejectionStatesTheAllowanceOnlyWhereItIsReported proves a refusal carries
// the allowance of a request that asked for it, and none for one that did not,
// which is answered with the decision alone.
func TestRejectionStatesTheAllowanceOnlyWhereItIsReported(t *testing.T) {
	t.Parallel()
	provider := rateRequest()
	provider.ReportRate = false
	for _, test := range []struct {
		name    string
		request Request
		reply   []any
		want    RateState
	}{
		{
			"a key", rateRequest(),
			rateReply(0, "rpm", 20_000, testWindow, 0, allowance{10, 0, 1000, 900, 20_000}),
			RateState{
				RequestLimit: 10, TokenLimit: 1000, TokenRemaining: 900,
				ResetAfter: 20 * time.Second, ResetAt: time.UnixMilli(testWindowEndMS).UTC(),
			},
		},
		{"a provider quota", provider, headReply(0, "rpm", 20_000, testWindow, 0), RateState{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lease, err := reserveAnswered(t, test.request, test.reply)
			exceeded, ok := errors.AsType[*ExceededError](err)
			if lease != nil || !ok || exceeded.Dimension != DimensionRequests || exceeded.Rate != test.want {
				t.Fatalf("Reserve = %v, %v, want a requests rejection stating %+v", lease, err, test.want)
			}
		})
	}
}

// TestLeaseStatesTheAllowanceTheRateScriptAnswered proves a gateway can read the
// request and token allowance off the lease, and only for the dimensions its key
// limits: a lease that reserved no request or token limit states none, so a
// non-nil lease is not by itself a reason to send rate-limit headers.
func TestLeaseStatesTheAllowanceTheRateScriptAnswered(t *testing.T) {
	t.Parallel()
	windowEnd := time.UnixMilli(1_789_383_660_000).UTC()
	for _, test := range []struct {
		name    string
		request func() Request
		reply   allowance
		want    RateState
	}{
		{
			"requests and tokens", rateRequest,
			allowance{10, 9, 1000, 995, 30_000},
			RateState{
				RequestLimit: 10, RequestRemaining: 9, TokenLimit: 1000, TokenRemaining: 995,
				ResetAfter: 30 * time.Second, ResetAt: windowEnd,
			},
		},
		{
			"requests only", func() Request { r := rateRequest(); r.TokensPerMinute, r.RequestedTokens = nil, 0; return r },
			allowance{requestLimit: 10, requestRemaining: 9, resetMS: 1},
			RateState{RequestLimit: 10, RequestRemaining: 9, ResetAfter: time.Millisecond, ResetAt: windowEnd},
		},
		{
			"tokens only", func() Request { r := rateRequest(); r.RequestsPerMinute = nil; return r },
			allowance{tokenLimit: 1000, tokenRemaining: 995, resetMS: 60_000},
			RateState{TokenLimit: 1000, TokenRemaining: 995, ResetAfter: time.Minute, ResetAt: windowEnd},
		},
		{
			"concurrency only", func() Request {
				r := rateRequest()
				r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens, r.MaxConcurrency = nil, nil, 0, pointer(int64(3))
				return r
			},
			allowance{}, RateState{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := test.request()
			expiry := int64(0)
			if request.MaxConcurrency != nil {
				expiry = 1_789_383_605_000
			}
			// A key that limits neither requests nor tokens is not answered with an
			// allowance, since there is none to state.
			answer := rateReply(1, "ok", 0, 29_823_060, expiry, test.reply)
			if !request.reportsRate() {
				answer = headReply(1, "ok", 0, 29_823_060, expiry)
			}
			lease, err := reserveAnswered(t, request, answer)
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			got := lease.RateState()
			if got != test.want {
				t.Fatalf("RateState = %+v, want %+v", got, test.want)
			}
			if got.Limited() != (test.want != RateState{}) ||
				got.LimitsRequests() != (request.RequestsPerMinute != nil) ||
				got.LimitsTokens() != (request.TokensPerMinute != nil) {
				t.Fatalf("RateState %+v does not report the dimensions %+v limits", got, request)
			}
		})
	}
}

func TestRateStateIsEmptyWhereTheRateScriptNeverRan(t *testing.T) {
	t.Parallel()
	var none *Lease
	if got := none.RateState(); got != (RateState{}) || got.Limited() {
		t.Fatalf("a nil lease states %+v", got)
	}
	server := &scripted{answer: answering(t, map[string][2]any{"reserve_cost": {costGranted, nil}})}
	lease, err := (&Limiter{client: server, namespace: "olp:test"}).Reserve(t.Context(), costRequest(false))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got := lease.RateState(); got != (RateState{}) || got.Limited() {
		t.Fatalf("a lease bound by cost alone states %+v", got)
	}
	// A group's lease carries a cost budget and its own allowance, and the key's
	// states only the key's.
	group := &Lease{rate: &rateAnswer{state: RateState{RequestLimit: 1, RequestRemaining: 1}}}
	lease.Attach(group)
	if got := lease.RateState(); got != (RateState{}) {
		t.Fatalf("an attached lease changed the key's allowance to %+v", got)
	}
	if got := group.RateState(); got.RequestLimit != 1 {
		t.Fatalf("attaching changed the group's allowance to %+v", got)
	}
}

// TestRateResetCountsTheMinuteDownFromTheScriptsReading proves a response written
// long after admission reports the time left in the minute, not the time that was
// left when the script measured it.
func TestRateResetCountsTheMinuteDownFromTheScriptsReading(t *testing.T) {
	t.Parallel()
	var none *Lease
	if got := none.RateReset(); got != 0 {
		t.Fatalf("a nil lease has %s left", got)
	}
	if got := (&Lease{}).RateReset(); got != 0 {
		t.Fatalf("a lease that states no allowance has %s left", got)
	}
	lease, err := reserveAnswered(t, rateRequest(), rateReply(1, "ok", 0, testWindow, 0, allowance{10, 9, 1000, 995, 30_000}))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// An answer that has just arrived is the reading itself.
	if got := lease.RateReset(); got > 30*time.Second || got < 29*time.Second {
		t.Fatalf("RateReset = %s just after the script measured 30s", got)
	}
	// One that arrived ten seconds ago has ten seconds fewer to run, never more
	// than the script measured, and none once the minute is over.
	lease.rate.at = time.Now().Add(-10 * time.Second)
	if got := lease.RateReset(); got > 20*time.Second || got < 19*time.Second {
		t.Fatalf("RateReset = %s ten seconds after the script measured 30s", got)
	}
	lease.rate.at = time.Now().Add(-time.Minute)
	if got := lease.RateReset(); got != 0 {
		t.Fatalf("RateReset = %s after the minute is over", got)
	}
	// The state itself stays what the script measured.
	if got := lease.RateState().ResetAfter; got != 30*time.Second {
		t.Fatalf("ResetAfter = %s, want what the script measured", got)
	}
}

// TestRateResetIsAWholeNumberOfMilliseconds proves the countdown is written the way
// the script's own reading is, whatever fraction of a millisecond has passed, and
// that it is rounded up, so a reading is never less than the time that is left
// and never states more than the script measured.
func TestRateResetIsAWholeNumberOfMilliseconds(t *testing.T) {
	t.Parallel()
	lease, err := reserveAnswered(t, rateRequest(), rateReply(1, "ok", 0, testWindow, 0, allowance{10, 9, 1000, 995, 30_000}))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	for _, passed := range []time.Duration{
		0, time.Nanosecond, 499 * time.Microsecond, time.Millisecond, 1500 * time.Microsecond,
		37*time.Millisecond + 496893*time.Nanosecond, 10*time.Second + 123456789*time.Nanosecond, 29 * time.Second,
	} {
		answered := time.Now().Add(-passed)
		lease.rate.at = answered
		got := lease.RateReset()
		// The clock keeps running while the reading is taken: it was at least
		// what the script measured less the time that had passed by its end, and at
		// most what remained at its start, rounded up.
		earliest := 30*time.Second - time.Since(answered)
		latest := 30*time.Second - passed
		if got%time.Millisecond != 0 || got > latest+time.Millisecond-1 || got < earliest || got > 30*time.Second {
			t.Errorf("%s after the answer: RateReset = %s (%d ns), want a whole number of milliseconds between %s and %s", passed, got, got.Nanoseconds(), earliest, latest)
		}
	}
}

// TestExceededErrorStatesTheAllowanceOfARateRejection proves a request refused by
// its rate limits tells the caller what the window still holds, because a refusal
// is a response too, and that a refusal by a cost budget, which never reached the
// rate script, states none.
func TestExceededErrorStatesTheAllowanceOfARateRejection(t *testing.T) {
	t.Parallel()
	windowEnd := time.UnixMilli(1_789_383_660_000).UTC()
	for _, test := range []struct {
		name      string
		reply     allowance
		detail    string
		dimension Dimension
		want      RateState
	}{
		{
			"requests", allowance{10, 0, 1000, 900, 20_000}, "rpm", DimensionRequests,
			RateState{
				RequestLimit: 10, TokenLimit: 1000, TokenRemaining: 900,
				ResetAfter: 20 * time.Second, ResetAt: windowEnd,
			},
		},
		{
			"tokens", allowance{10, 2, 1000, 3, 20_000}, "tpm", DimensionTokens,
			RateState{
				RequestLimit: 10, RequestRemaining: 2, TokenLimit: 1000, TokenRemaining: 3,
				ResetAfter: 20 * time.Second, ResetAt: windowEnd,
			},
		},
		{
			"concurrency", allowance{10, 2, 1000, 3, 30_000}, "concurrency", DimensionConcurrency,
			RateState{
				RequestLimit: 10, RequestRemaining: 2, TokenLimit: 1000, TokenRemaining: 3,
				ResetAfter: 30 * time.Second, ResetAt: windowEnd,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			retry := int64(20_000)
			if test.dimension == DimensionConcurrency {
				retry = 4_000
			}
			lease, err := reserveAnswered(t, rateRequest(), rateReply(0, test.detail, retry, 29_823_060, 0, test.reply))
			exceeded, ok := errors.AsType[*ExceededError](err)
			if lease != nil || !ok || exceeded.Dimension != test.dimension {
				t.Fatalf("Reserve = %v, %v, want a %s rejection", lease, err, test.dimension)
			}
			if exceeded.Rate != test.want {
				t.Fatalf("Rate = %+v, want %+v", exceeded.Rate, test.want)
			}
		})
	}
	server := &scripted{answer: answering(t, map[string][2]any{"reserve_cost": {costRefusing, nil}})}
	_, err := (&Limiter{client: server, namespace: "olp:test"}).Reserve(t.Context(), costRequest(true))
	exceeded, ok := errors.AsType[*ExceededError](err)
	if !ok || exceeded.Rate != (RateState{}) || server.ran(reserveLimitsScript) != 0 {
		t.Fatalf("a cost rejection = %+v, %v, want none from a rate script that never ran", exceeded, err)
	}
}

// TestReserveRefusesAnAnswerForOtherLimits proves the limits the script echoes are
// checked against the ones it was asked to enforce: an answer that states other
// limits was made for another request, and admitting or refusing on it would be a
// guess.
func TestReserveRefusesAnAnswerForOtherLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		reply []any
	}{
		{"a granted request limited otherwise", rateReply(1, "ok", 0, 5, 0, allowance{20, 9, 1000, 995, 30_000})},
		{"a granted token limit limited otherwise", rateReply(1, "ok", 0, 5, 0, allowance{10, 9, 2000, 995, 30_000})},
		{"a granted request limit that is none", rateReply(1, "ok", 0, 5, 0, allowance{0, 0, 1000, 995, 30_000})},
		{"a granted token limit that is none", rateReply(1, "ok", 0, 5, 0, allowance{10, 9, 0, 0, 30_000})},
		{"a refused request limited otherwise", rateReply(0, "rpm", 1000, 5, 0, allowance{20, 0, 1000, 995, 1000})},
		{"a refused token limit limited otherwise", rateReply(0, "tpm", 1000, 5, 0, allowance{10, 5, 2000, 0, 1000})},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lease, err := reserveAnswered(t, rateRequest(), test.reply)
			if lease != nil || !isUnexpected(err) {
				t.Fatalf("Reserve = %v, %v, want an unexpected response", lease, err)
			}
		})
	}
}

// slowServer is a Commander in front of another that takes latency to answer
// every command. It does not sleep: a command given less time than that fails the
// way a timeout does, and any other is answered at once, which keeps a test of what
// a server this slow does to the limiter fast and exact. It keeps the time each
// settlement script call was given.
type slowServer struct {
	Commander
	latency time.Duration
	mu      sync.Mutex
	allowed []time.Duration
}

func (s *slowServer) Do(ctx context.Context, args ...string) (any, error) {
	allowed := time.Hour
	if deadline, ok := ctx.Deadline(); ok {
		allowed = time.Until(deadline)
	}
	if len(args) > 1 && (args[1] == settleCostScript.digest || args[1] == settleCostScript.source) {
		s.mu.Lock()
		s.allowed = append(s.allowed, allowed)
		s.mu.Unlock()
	}
	if allowed < s.latency {
		return nil, lostReply
	}
	return s.Commander.Do(ctx, args...)
}

func (s *slowServer) settlements() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.allowed)
}

// TestReleasingAnEstimateOutlastsASlowServer proves the release of an estimate
// that nothing will settle is given as long as every other release, not as long
// as a settlement is. Admission tolerates a second, so a server that answers in
// 120 to 200 milliseconds admits a request; if the release of its estimate then
// gave up first, the whole estimate would be held until the reservation lapsed.
func TestReleasingAnEstimateOutlastsASlowServer(t *testing.T) {
	t.Parallel()
	for _, latency := range []time.Duration{120 * time.Millisecond, 200 * time.Millisecond} {
		answers := map[string][2]any{
			"reserve_cost": {costGranted, nil}, "reserve_limits": {rateGranted, nil},
			"refund_limits": {costSettled, nil}, "settle_cost": {costSettled, nil},
		}
		for name, run := range map[string]func(t *testing.T, limiter *Limiter, inner *scripted){
			"a refund": func(t *testing.T, limiter *Limiter, _ *scripted) {
				lease, err := limiter.Reserve(t.Context(), costRequest(true))
				if err != nil {
					t.Fatalf("Reserve: %v", err)
				}
				if err := lease.Refund(t.Context()); err != nil {
					t.Fatalf("Refund: %v", err)
				}
			},
			"a refund of a request that holds only a cost budget": func(t *testing.T, limiter *Limiter, _ *scripted) {
				lease, err := limiter.Reserve(t.Context(), costRequest(false))
				if err != nil {
					t.Fatalf("Reserve: %v", err)
				}
				if err := lease.Refund(t.Context()); err != nil {
					t.Fatalf("Refund: %v", err)
				}
			},
			// A request that cost nothing, such as one whose usage is still to come,
			// settles by giving its estimate back, and nothing else will.
			"a settlement to nothing": func(t *testing.T, limiter *Limiter, _ *scripted) {
				lease, err := limiter.Reserve(t.Context(), costRequest(false))
				if err != nil {
					t.Fatalf("Reserve: %v", err)
				}
				lease.SetActualCost("0")
				if err := lease.SettleCost(t.Context()); err != nil {
					t.Fatalf("SettleCost: %v", err)
				}
			},
			"an estimate whose reservation reply was lost": func(t *testing.T, limiter *Limiter, inner *scripted) {
				inner.answer = answering(t, map[string][2]any{"reserve_cost": {nil, lostReply}, "settle_cost": {costSettled, nil}})
				if lease, err := limiter.Reserve(t.Context(), costRequest(false)); lease != nil || !isService(err) {
					t.Fatalf("Reserve = %v, %v, want the outage", lease, err)
				}
			},
		} {
			t.Run(latency.String()+" "+name, func(t *testing.T) {
				t.Parallel()
				inner := &scripted{answer: answering(t, answers)}
				server := &slowServer{Commander: inner, latency: latency}
				run(t, &Limiter{client: server, namespace: "olp:test"}, inner)
				if got := inner.ran(settleCostScript); got != 1 {
					t.Fatalf("the estimate was released %d times, want it released once", got)
				}
				for _, allowed := range server.settlements() {
					if allowed <= settleTimeout {
						t.Fatalf("a release was given %v, no more than the %v of a settlement", allowed, settleTimeout)
					}
				}
			})
		}
	}
}

// TestSettlingToNothingIsRetriedLikeARelease proves a cost of nothing is released
// the way every release is, with an attempt more when one fails, and that what is
// not nothing is not.
func TestSettlingToNothingIsRetriedLikeARelease(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		actual   string
		attempts int
	}{
		{"0", cleanupAttempts}, {"0.000", cleanupAttempts}, {"00.0", cleanupAttempts}, {"0.000000000001", 1}, {"10", 1},
	} {
		t.Run(test.actual, func(t *testing.T) {
			t.Parallel()
			server := &scripted{answer: answering(t, map[string][2]any{
				"reserve_cost": {costGranted, nil}, "settle_cost": {nil, refusedReply},
			})}
			lease, err := (&Limiter{client: server, namespace: "olp:test"}).Reserve(t.Context(), costRequest(false))
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			lease.SetActualCost(test.actual)
			if err := lease.SettleCost(t.Context()); err == nil {
				t.Fatal("SettleCost succeeded against a server that refuses every call")
			}
			if got := server.ran(settleCostScript); got != test.attempts {
				t.Fatalf("a settlement to %s made %d attempts, want %d", test.actual, got, test.attempts)
			}
		})
	}
}

func TestZeroAmount(t *testing.T) {
	t.Parallel()
	for amount, want := range map[string]bool{
		"0": true, "00": true, "0.0": true, "0.000000000000": true, "000.000": true,
		"": false, ".": false, "1": false, "0.1": false, "10": false, "0..0": false, "0.0.0": false, "0a": false,
	} {
		if got := zeroAmount(amount); got != want {
			t.Errorf("zeroAmount(%q) = %t, want %t", amount, got, want)
		}
	}
}

// TestSettlingACostIsOneBoundedAttempt proves the other direction: a settlement,
// which is the step whose loss costs least, is a single attempt that gives up
// after settleTimeout, and a server that is quick enough still gets it.
func TestSettlingACostIsOneBoundedAttempt(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		latency time.Duration
		settled bool
	}{
		{"a server quicker than the bound", settleTimeout / 2, true},
		{"a server slower than the bound", settleTimeout + 50*time.Millisecond, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inner := &scripted{answer: answering(t, map[string][2]any{
				"reserve_cost": {costGranted, nil}, "settle_cost": {costSettled, nil},
			})}
			server := &slowServer{Commander: inner, latency: test.latency}
			lease, err := (&Limiter{client: server, namespace: "olp:test"}).Reserve(t.Context(), costRequest(false))
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			lease.SetActualCost("0.1")
			err = lease.SettleCost(t.Context())
			if (err == nil) != test.settled {
				t.Fatalf("SettleCost = %v, want settled = %t", err, test.settled)
			}
			allowed := server.settlements()
			if len(allowed) != 1 || allowed[0] > settleTimeout {
				t.Fatalf("a settlement made %d attempts, given %v, want one given no more than %v", len(allowed), allowed, settleTimeout)
			}
			if got := inner.ran(settleCostScript); (got == 1) != test.settled {
				t.Fatalf("the script ran %d times, want it to run only for a settlement that was in time", got)
			}
		})
	}
}

// TestReserveAsksOnlyTheScriptsABudgetNeeds proves a key pays for the budgets it
// has and no others: no cost script for a key with only rate limits, no rate
// script for one with only a cost budget, and no script at all for neither.
func TestReserveAsksOnlyTheScriptsABudgetNeeds(t *testing.T) {
	t.Parallel()
	cost := func() Request { r := costRequest(false); r.CostEstimate, r.RequestID = "", ""; return r }
	rate := func() Request {
		r := costRequest(true)
		r.DailyCostLimit, r.CostEstimate, r.RequestID = nil, "", ""
		return r
	}
	both := func() Request { r := costRequest(true); r.CostEstimate, r.RequestID = "", ""; return r }
	for _, test := range []struct {
		name      string
		request   Request
		wantCost  int
		wantRates int
	}{
		{"a cost budget alone", cost(), 1, 0},
		{"a rate limit alone", rate(), 0, 1},
		{"both", both(), 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := &scripted{answer: answering(t, map[string][2]any{
				"reserve_cost": {costGranted, nil}, "reserve_limits": {rateGranted, nil},
			})}
			limiter := &Limiter{client: server, namespace: "olp:test"}
			if _, err := limiter.Reserve(t.Context(), test.request); err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			if got := server.ran(reserveCostScript); got != test.wantCost {
				t.Fatalf("the cost script ran %d times, want %d", got, test.wantCost)
			}
			if got := server.ran(reserveLimitsScript); got != test.wantRates {
				t.Fatalf("the rate script ran %d times, want %d", got, test.wantRates)
			}
		})
	}
}

// TestKeysForNamesOnlyTheKeysABudgetUses proves a request with no cost budget is
// not made to build the cost keys of an owner, and one with no rate limit not the
// rate keys: a feature nobody configured allocates nothing.
//
// It counts allocations, which is global to the process, so it does not run in
// parallel with the tests that allocate.
func TestKeysForNamesOnlyTheKeysABudgetUses(t *testing.T) {
	limiter := &Limiter{namespace: "olp:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits"}
	const owner = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	rateOnly := Request{CostOwnerID: owner, LookupID: "lookup_one_abc", RequestsPerMinute: pointer(int64(1))}
	costOnly := Request{CostOwnerID: owner, LookupID: "lookup_one_abc", MonthlyCostLimit: pointer("1")}

	if got := limiter.keysFor(Request{CostOwnerID: owner, LookupID: "lookup_one_abc"}); got != (keys{}) {
		t.Fatalf("a request with no budget was given keys: %+v", got)
	}
	got := limiter.keysFor(rateOnly)
	if got.rate == "" || got.concurrency == "" || got.dailyCost != "" || got.monthlyCost != "" || got.pending != "" || got.expiry != "" {
		t.Fatalf("a rate-limited request was given %+v, want its rate keys alone", got)
	}
	got = limiter.keysFor(costOnly)
	if got.rate != "" || got.concurrency != "" || got.dailyCost == "" || got.monthlyCost == "" || got.pending == "" || got.expiry == "" {
		t.Fatalf("a cost-budgeted request was given %+v, want its cost keys alone", got)
	}

	allocations := func(f func()) float64 { return testing.AllocsPerRun(200, f) }
	var sink keys
	rateKeysAlone := allocations(func() {
		rate, concurrency := limiter.rateKeys("lookup_one_abc")
		sink.rate, sink.concurrency = rate, concurrency
	})
	if got := allocations(func() { sink = limiter.keysFor(rateOnly) }); got > rateKeysAlone {
		t.Fatalf("a rate-limited request allocates %.0f times, more than the %.0f its own keys take", got, rateKeysAlone)
	}
	if got := allocations(func() { sink = limiter.keysFor(Request{CostOwnerID: owner, LookupID: "lookup_one_abc"}) }); got != 0 {
		t.Fatalf("a request with no budget allocates %.0f times, want none", got)
	}
	_ = sink
}

func TestNewRefusesAClientThatIsNotThere(t *testing.T) {
	t.Parallel()
	var missing *coordination.Client
	for name, client := range map[string]Commander{"no client": nil, "a nil client": missing} {
		if _, err := New(client, "limits"); err == nil || !strings.Contains(err.Error(), "client is required") {
			t.Fatalf("New with %s: %v, want a refusal", name, err)
		}
	}
	if _, err := New(&scripted{}, "limits"); err != nil {
		t.Fatalf("New over a Commander: %v", err)
	}
}

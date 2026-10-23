package limits

import (
	"context"
	"errors"
	"slices"
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

	costGranted = reply(int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000))
	costSettled = reply(int64(1), int64(1), "ok", int64(1), int64(0))
)

// The rate replies answer costRequest(true), which limits requests to ten and
// does not ask for the allowance, so they are the decision alone.
var (
	rateGranted = headReply(1, "ok", 0, 5, 0)
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

// The server holds the script now, so the next call names it and no more.

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

// It is released under the request it was made for, with nothing left to
// hold, by one bounded attempt and not a series.

func isService(err error) bool {
	_, ok := errors.AsType[*ServiceError](err)
	return ok
}

// The same request answered the other way is not the script that was run.

// A key that limits neither requests nor tokens is not answered with an
// allowance, since there is none to state.

// A group's lease carries a cost budget and its own allowance, and the key's
// states only the key's.

// An answer that has just arrived is the reading itself.

// One that arrived ten seconds ago has ten seconds fewer to run, never more
// than the script measured, and none once the minute is over.

// The state itself stays what the script measured.

// The clock keeps running while the reading is taken: it was at least
// what the script measured less the time that had passed by its end, and at
// most what remained at its start, rounded up.

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
				lease.SetActualCost("0", "0")
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

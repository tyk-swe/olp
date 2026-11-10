package admission

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

const (
	owner     = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	requestID = "0192CF87-D4AB-7F2E-A8B1-C2D3E4F50608"
)

var (
	lostReply   = &coordination.CommandError{Cause: context.DeadlineExceeded, Ambiguous: true}
	costGranted = []any{int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000)}
	costSettled = []any{int64(1), int64(1), "ok", int64(1), int64(0)}
	rateGranted = []any{int64(2), int64(1), "ok", int64(0), int64(5), int64(0)}
)

type answer struct {
	value any
	err   error
}

type server struct {
	mu      sync.Mutex
	asked   [][]string
	answers map[string]answer
}

func touched(args []string) string {
	switch {
	case len(args) < 4:
		return ""
	case strings.HasSuffix(args[3], ":pending"):
		return "settle_cost"
	case strings.HasSuffix(args[3], ":day"):
		return "reserve_cost"
	case strings.HasSuffix(args[3], ":rate") && len(args) >= 12:
		return "reserve_rate"
	case strings.HasSuffix(args[3], ":rate"):
		return "refund_rate"
	}
	return ""
}

func (s *server) Do(_ context.Context, args ...string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, slices.Clone(args))
	found, ok := s.answers[touched(args)]
	if !ok {
		return nil, &coordination.CommandError{Cause: errors.New("connection refused")}
	}
	return found.value, found.err
}

func (s *server) sent(name string) [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.asked), func(args []string) bool { return touched(args) != name })
}

type slowServer struct {
	scripts.Commander
	latency time.Duration
	mu      sync.Mutex
	allowed []time.Duration
}

func (s *slowServer) Do(ctx context.Context, args ...string) (any, error) {
	allowed := time.Hour
	if deadline, ok := ctx.Deadline(); ok {
		allowed = time.Until(deadline)
	}
	if touched(args) == "settle_cost" {
		s.mu.Lock()
		s.allowed = append(s.allowed, allowed)
		s.mu.Unlock()
	}
	if allowed < s.latency {
		return nil, lostReply
	}
	return s.Commander.Do(ctx, args...)
}

func limiterOn(t *testing.T, client scripts.Commander) *Limiter {
	t.Helper()
	runner, err := scripts.NewRunner(client, "olp:test")
	if err != nil {
		t.Fatal(err)
	}
	return New(runner)
}

func costRequest(basis limits.Basis, rate bool) Request {
	budget := Budget(limits.Owner{ID: owner, Kind: limits.OwnerKey, Basis: basis}, pointer("10"), nil, nil, "")
	r := Request{Lookup: "lookup_one_abc", LeaseTTL: 5 * time.Second, Cost: budget.Holding("0.25", requestID)}
	if rate {
		r.Rate.RequestsPerMinute = pointer(int64(10))
	}
	return r
}

func TestReleasingAnEstimateOutlastsASlowServer(t *testing.T) {
	t.Parallel()
	granted := map[string]answer{"reserve_cost": {value: costGranted}, "reserve_rate": {value: rateGranted}, "refund_rate": {value: costSettled}, "settle_cost": {value: costSettled}}
	for _, latency := range []time.Duration{120 * time.Millisecond, 200 * time.Millisecond} {
		for name, test := range map[string]struct {
			answers map[string]answer
			run     func(t *testing.T, limiter *Limiter)
		}{
			"a refund": {granted, func(t *testing.T, limiter *Limiter) {
				lease, err := limiter.Reserve(t.Context(), costRequest(limits.BasisCost, true))
				if err != nil {
					t.Fatalf("Reserve: %v", err)
				}
				if err := lease.Refund(t.Context()); err != nil {
					t.Fatalf("Refund: %v", err)
				}
			}},
			"a refund of a request that holds only a cost budget": {granted, func(t *testing.T, limiter *Limiter) {
				lease, err := limiter.Reserve(t.Context(), costRequest(limits.BasisCost, false))
				if err != nil {
					t.Fatalf("Reserve: %v", err)
				}
				if err := lease.Refund(t.Context()); err != nil {
					t.Fatalf("Refund: %v", err)
				}
			}},
			"a settlement to nothing": {granted, func(t *testing.T, limiter *Limiter) {
				lease, err := limiter.Reserve(t.Context(), costRequest(limits.BasisCost, false))
				if err != nil {
					t.Fatalf("Reserve: %v", err)
				}
				lease.SetActualCost("0", "0")
				if err := lease.SettleCost(t.Context()); err != nil {
					t.Fatalf("SettleCost: %v", err)
				}
			}},
			"an estimate whose reservation reply was lost": {map[string]answer{"reserve_cost": {err: lostReply}, "settle_cost": {value: costSettled}}, func(t *testing.T, limiter *Limiter) {
				lease, err := limiter.Reserve(t.Context(), costRequest(limits.BasisCost, false))
				if _, outage := errors.AsType[*limits.ServiceError](err); lease != nil || !outage {
					t.Fatalf("Reserve = %v, %v, want the outage", lease, err)
				}
			}},
		} {
			t.Run(latency.String()+" "+name, func(t *testing.T) {
				t.Parallel()
				inner := &server{answers: test.answers}
				slow := &slowServer{Commander: inner, latency: latency}
				test.run(t, limiterOn(t, slow))
				if got := len(inner.sent("settle_cost")); got != 1 {
					t.Fatalf("the estimate was released %d times, want it released once", got)
				}
				for _, allowed := range slow.allowed {
					if allowed <= settleTimeout {
						t.Fatalf("a release was given %v, no more than the %v of a settlement", allowed, settleTimeout)
					}
				}
			})
		}
	}
}

func TestChargeBasisSettlesItsOwnReservation(t *testing.T) {
	t.Parallel()
	inner := &server{answers: map[string]answer{"reserve_cost": {value: costGranted}, "settle_cost": {value: costSettled}}}
	lease, err := limiterOn(t, inner).Reserve(t.Context(), costRequest(limits.BasisCharge, false))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := lease.GrowCost(t.Context(), "0.3", "0.7", requestID, time.Second); err != nil {
		t.Fatalf("GrowCost: %v", err)
	}
	lease.SetActualCost("0.25", "0.55")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	reserved, settled := inner.sent("reserve_cost"), inner.sent("settle_cost")
	if len(reserved) != 2 || len(settled) != 1 {
		t.Fatalf("reserved %d times and settled %d times, want 2 and 1", len(reserved), len(settled))
	}
	if grown := strings.Join(reserved[1], " "); !strings.Contains(grown, ":charge:pending") || !strings.Contains(grown, " 0.7 ") {
		t.Fatalf("the estimate grew on %q, want the charge amount on the charge keys", grown)
	}
	if actual := strings.Join(settled[0], " "); !strings.Contains(actual, ":charge:pending") || !strings.Contains(actual, " 0.55 ") {
		t.Fatalf("the actual settled %q, want the charge amount on the charge keys", actual)
	}
}

func TestReserveRejectsBypassableLimits(t *testing.T) {
	t.Parallel()
	base := func() Request {
		return Request{
			Lookup: "lookup_one_abc", LeaseTTL: 5 * time.Second,
			Rate: RateLimits{RequestsPerMinute: pointer(int64(10)), TokensPerMinute: pointer(int64(1000)), MaxConcurrency: pointer(int64(10)), Tokens: 5},
			Cost: Budget(limits.Owner{ID: owner, Kind: limits.OwnerKey}, pointer("10"), nil, pointer("100"), ""),
		}
	}
	reserve := func(r Request) error {
		_, err := limiterOn(t, &server{answers: map[string]answer{"reserve_cost": {value: costGranted}, "reserve_rate": {value: []any{int64(2), int64(1), "ok", int64(0), int64(5), int64(1)}}}}).Reserve(t.Context(), r)
		return err
	}
	accepted := append([]Request{base()}, func() []Request {
		connection, slot := base(), base()
		connection.Lookup, slot.Lookup = limits.ConnectionLookup(owner), limits.SlotLookup(owner)
		return []Request{connection, slot}
	}()...)
	for _, request := range accepted {
		if err := reserve(request); err != nil {
			t.Fatalf("Reserve rejected the lookup %q: %v", request.Lookup, err)
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*Request)
	}{
		{"zero requests per minute", func(r *Request) { r.Rate.RequestsPerMinute = pointer(int64(0)) }},
		{"negative tokens per minute", func(r *Request) { r.Rate.TokensPerMinute = pointer(int64(-1)) }},
		{"negative requested tokens", func(r *Request) { r.Rate.Tokens = -1 }},
		{"no tokens requested under a token limit", func(r *Request) { r.Rate.Tokens = 0 }},
		{"concurrency beyond the Lua range", func(r *Request) { r.Rate.MaxConcurrency = pointer(limits.MaxCounter + 1) }},
		{"requested tokens beyond the Lua range", func(r *Request) { r.Rate.Tokens = limits.MaxCounter + 1 }},
		{"zero lease TTL", func(r *Request) { r.LeaseTTL = 0 }},
		{"sub-millisecond lease TTL", func(r *Request) { r.LeaseTTL = 999 * time.Microsecond }},
		{"lookup with a hash tag", func(r *Request) { r.Lookup = "bad}{slot" }},
		{"lookup too short", func(r *Request) { r.Lookup = "short" }},
		{"lookup too long", func(r *Request) { r.Lookup = strings.Repeat("a", 41) }},
		{"lookup with a dash", func(r *Request) { r.Lookup = "lookup-one-abc" }},
		{"cost owner that is not a UUID", func(r *Request) { r.Cost.Owner.ID = "not-a-uuid" }},
		{"zero daily cost limit", func(r *Request) { r.Cost.Daily = pointer("0.000") }},
		{"negative monthly cost limit", func(r *Request) { r.Cost.Monthly = pointer("-1") }},
		{"cost limit in scientific notation", func(r *Request) { r.Cost.Daily = pointer("1e3") }},
		{"an estimate without the request it is accounted under", func(r *Request) { r.Cost.Estimate = &Estimate{Amount: "1"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := base()
			test.mutate(&request)
			if _, invalid := errors.AsType[*limits.InvalidRequestError](reserve(request)); !invalid {
				t.Fatalf("Reserve accepted %+v", request)
			}
		})
	}
}

func pointer[T any](value T) *T { return &value }

type syncing func(ownerID string) error

func (s syncing) SyncOwner(_ context.Context, ownerID string) error { return s(ownerID) }

func TestDurableAdmissionRebuildsOnlyAMissingProjection(t *testing.T) {
	t.Parallel()
	state := func(detail string) answer {
		return answer{value: []any{int64(1), int64(-1), detail, int64(0), int64(1), int64(2)}}
	}
	unavailable := errors.New("ledger unavailable")
	for name, test := range map[string]struct {
		reply  answer
		sync   error
		synced int
		want   error
	}{
		"a missing projection the ledger cannot rebuild": {reply: state("uninitialized_durable_cost_state"), sync: unavailable, synced: 1, want: unavailable},
		"a missing projection that stays missing":        {reply: state("uninitialized_durable_cost_state"), synced: 1, want: limits.ErrUninitializedCost},
		"an unresolved overrun":                          {reply: state("unsettled_durable_cost_state"), want: limits.ErrUninitializedCost},
		"a warm projection":                              {reply: answer{value: costGranted}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, err := scripts.NewRunner(&server{answers: map[string]answer{"reserve_cost": test.reply}}, "olp:test")
			if err != nil {
				t.Fatal(err)
			}
			synced := 0
			limiter := NewDurable(runner, syncing(func(ownerID string) error {
				if ownerID != owner {
					t.Errorf("synchronized owner %q, want %q", ownerID, owner)
				}
				synced++
				return test.sync
			}))
			budget := Budget(limits.Owner{ID: owner, Kind: limits.OwnerKey}, pointer("10"), nil, nil, "")
			_, err = limiter.Reserve(t.Context(), Request{Lookup: "lookup_one_abc", LeaseTTL: time.Second, Cost: budget})
			if !errors.Is(err, test.want) || (test.want == nil) != (err == nil) {
				t.Fatalf("Reserve error = %v, want %v", err, test.want)
			}
			if synced != test.synced {
				t.Fatalf("the ledger was asked to rebuild %d times, want %d", synced, test.synced)
			}
		})
	}
}

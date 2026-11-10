//go:build integration

package integration_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/tests/harness"
)

func newRunner(t *testing.T) scripts.Runner {
	t.Helper()
	runner, err := scripts.NewRunner(harness.Valkey(t), "olp:"+uuid.NewString()+":")
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func rated(rate admission.RateLimits) admission.Request {
	return admission.Request{Lookup: limits.RouteLookup(uuid.NewString()), LeaseTTL: time.Minute, Rate: rate}
}

func awayFromMinuteRollover() {
	if left := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)); left < 2*time.Second {
		time.Sleep(left)
	}
}

func refusal(err error) limits.ExceededError {
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		return *exceeded
	}
	return limits.ExceededError{}
}

func contend(t *testing.T, limiter *admission.Limiter, request admission.Request, contenders int) []*admission.Lease {
	t.Helper()
	type outcome struct {
		lease *admission.Lease
		err   error
	}
	outcomes := make(chan outcome, contenders)
	var group sync.WaitGroup
	for range contenders {
		group.Go(func() {
			lease, err := limiter.Reserve(t.Context(), request)
			outcomes <- outcome{lease, err}
		})
	}
	group.Wait()
	close(outcomes)
	admitted := make([]*admission.Lease, 0, contenders)
	for o := range outcomes {
		switch {
		case o.err == nil:
			admitted = append(admitted, o.lease)
		case refusal(o.err).Dimension != limits.DimensionConcurrency:
			t.Errorf("got %v, want a concurrency refusal", o.err)
		}
	}
	return admitted
}

func TestConcurrencyLimitAdmitsExactlyItsCapacityUnderContention(t *testing.T) {
	t.Parallel()
	const capacity, contenders = 5, 40
	limiter := admission.New(newRunner(t))
	request := rated(admission.RateLimits{MaxConcurrency: new(int64(capacity))})
	first := contend(t, limiter, request, contenders)
	if len(first) != capacity {
		t.Fatalf("%d of %d contenders were admitted, want %d", len(first), contenders, capacity)
	}
	for _, lease := range first {
		for range 2 {
			if err := lease.Release(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if again := contend(t, limiter, request, contenders); len(again) != capacity {
		t.Errorf("%d contenders were admitted after every slot was released twice, want %d", len(again), capacity)
	}
}

func TestAbandonedConcurrencySlotComesBackWhenItsLeaseLapses(t *testing.T) {
	t.Parallel()
	limiter := admission.New(newRunner(t))
	request := rated(admission.RateLimits{MaxConcurrency: new(int64(2))})
	abandoned := request
	abandoned.LeaseTTL = 50 * time.Millisecond
	for _, held := range []admission.Request{abandoned, request} {
		if _, err := limiter.Reserve(t.Context(), held); err != nil {
			t.Fatal(err)
		}
	}
	harness.Eventually(t, "the abandoned slot to come back", 5*time.Second, func() (bool, string) {
		_, err := limiter.Reserve(t.Context(), request)
		return err == nil, fmt.Sprint(err)
	})
	if _, err := limiter.Reserve(t.Context(), request); refusal(err).Dimension != limits.DimensionConcurrency {
		t.Errorf("got %v with both slots held again, want a concurrency refusal", err)
	}
}

func TestRequestWindowRefusesTheOverflowAndGivesBackRefunds(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	limiter := admission.New(newRunner(t))
	request := rated(admission.RateLimits{RequestsPerMinute: new(int64(3)), Report: true})
	reserve := func() *admission.Lease {
		lease, err := limiter.Reserve(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	first := reserve()
	for _, remaining := range []int64{1, 0} {
		if state := reserve().RateState(); state.RequestLimit != 3 || state.RequestRemaining != remaining {
			t.Fatalf("got %+v, want %d of 3 requests remaining", state, remaining)
		}
	}
	_, err := limiter.Reserve(t.Context(), request)
	if refused := refusal(err); refused.Dimension != limits.DimensionRequests || refused.RetryAfter <= 0 || refused.RetryAfter > time.Minute {
		t.Fatalf("got %v, want a request refusal that ends within the minute", err)
	}
	if err := first.Refund(t.Context()); err != nil {
		t.Fatal(err)
	}
	reserve()
	if _, err := limiter.Reserve(t.Context(), request); refusal(err).Dimension != limits.DimensionRequests {
		t.Errorf("got %v after the refunded request was used again, want a request refusal", err)
	}
}

func TestTokenWindowChargesWhatARequestActuallyUsed(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	limiter := admission.New(newRunner(t))
	request := rated(admission.RateLimits{TokensPerMinute: new(int64(100)), Tokens: 40})
	asking := func(tokens int64) admission.Request {
		sized := request
		sized.Rate.Tokens = tokens
		return sized
	}
	lease, err := limiter.Reserve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Reconcile(t.Context(), 70); err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Reserve(t.Context(), asking(31)); refusal(err).Dimension != limits.DimensionTokens {
		t.Fatalf("got %v for 31 tokens after 70 of 100 were used, want a token refusal", err)
	}
	if _, err := limiter.Reserve(t.Context(), asking(30)); err != nil {
		t.Errorf("got %v for the 30 tokens that were left", err)
	}
}

func TestSupplyReportsEachLookupsTightestHeadroom(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	limiter := admission.New(newRunner(t))
	busy := rated(admission.RateLimits{RequestsPerMinute: new(int64(4)), TokensPerMinute: new(int64(100)), Tokens: 10})
	for range 3 {
		if _, err := limiter.Reserve(t.Context(), busy); err != nil {
			t.Fatal(err)
		}
	}
	probe := func(lookup string) admission.QuotaProbe {
		return admission.QuotaProbe{Lookup: lookup, RequestsPerMinute: busy.Rate.RequestsPerMinute, TokensPerMinute: busy.Rate.TokensPerMinute}
	}
	headroom, _ := limiter.Supply(t.Context(), []admission.QuotaProbe{probe(busy.Lookup), probe(limits.RouteLookup(uuid.NewString()))}, nil)
	want := []admission.Headroom{{Fraction: 0.25, Known: true}, {Fraction: 1, Known: true}}
	if !slices.Equal(headroom, want) {
		t.Errorf("got %+v, want %+v for 3 of 4 requests used and for an idle lookup", headroom, want)
	}
}

func TestBudgetExposureSurvivesLosingValkey(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	store := debits.NewStore(pool)
	owner := limits.Owner{ID: fixture.keyID, Kind: limits.OwnerKey, Basis: limits.BasisCost}
	boundaries := []debits.Boundary{{Owner: owner, Budget: admission.Budget(owner, new("1.00"), nil, nil, "")}}
	admit := func(syncer *debits.Syncer, bound string) (debits.Intent, error) {
		intent := debits.Intent{
			ID: uuid.NewString(), APIKeyID: fixture.keyID, ProjectID: fixture.projectID, Kind: debits.KindAPI, ClaimID: uuid.NewString(),
			Affinity: debits.Affinity{ReleaseID: fixture.generationID, RevisionID: uuid.NewString()},
			Currency: "USD", CostBound: bound, ChargeBound: bound, PreparedUntil: time.Now().Add(time.Minute),
		}
		if _, err := store.Prepare(t.Context(), intent, []limits.Owner{owner}); err != nil {
			t.Fatal(err)
		}
		return intent, syncer.Admit(t.Context(), intent.ID, intent.ClaimID, boundaries)
	}
	release := func(intent debits.Intent) {
		if err := store.ReleaseUndispatched(t.Context(), intent.ID, intent.ClaimID); err != nil {
			t.Fatal(err)
		}
	}
	syncer := debits.NewSyncer(store, newRunner(t))
	held, err := admit(syncer, "0.60")
	if err != nil {
		t.Fatal(err)
	}
	over, err := admit(syncer, "0.60")
	if refusal(err).Dimension != limits.DimensionDailyCost {
		t.Fatalf("got %v for 0.60 on top of 0.60 held under a 1.00 budget, want a daily cost refusal", err)
	}
	release(over)
	if _, err := admit(syncer, "0.40"); err != nil {
		t.Fatalf("got %v for the 0.40 that was left", err)
	}

	recovered := debits.NewSyncer(store, newRunner(t))
	lost, err := admit(recovered, "0.10")
	if refusal(err).Dimension != limits.DimensionDailyCost {
		t.Fatalf("got %v on an empty Valkey, want the 1.00 held in PostgreSQL to refuse 0.10 more", err)
	}
	release(lost)
	release(held)
	if _, err := admit(recovered, "0.60"); err != nil {
		t.Errorf("got %v for the 0.60 that a release gave back", err)
	}
}

func TestCostBudgetCountsHeldEstimatesAndSettledActuals(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	owner := limits.Owner{ID: fixture.keyID, Kind: limits.OwnerKey, Basis: limits.BasisCost}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(ledger.OpenFreshOwner(t.Context(), tx, owner.Kind, owner.ID), tx.Commit(t.Context())); err != nil {
		t.Fatal(err)
	}
	runner := newRunner(t)
	limiter := admission.NewDurable(runner, debits.NewSyncer(debits.NewStore(pool), runner))
	budget := admission.Budget(owner, new("1.00"), nil, nil, "")
	holding := func(amount string) (*admission.Lease, error) {
		return limiter.Reserve(t.Context(), admission.Request{
			Lookup: limits.RouteLookup(uuid.NewString()), LeaseTTL: time.Minute, Cost: budget.Holding(amount, uuid.NewString()),
		})
	}
	first, err := holding("0.60")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holding("0.60"); refusal(err).Dimension != limits.DimensionDailyCost || !refusal(err).Estimate {
		t.Fatalf("got %v for 0.60 on top of a held 0.60 under a 1.00 budget, want a daily cost refusal of the estimate", err)
	}
	first.SetActualCost("0.10", "0.10")
	if err := first.SettleCost(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := holding("0.60")
	if err != nil {
		t.Fatalf("got %v for 0.60 after the first request settled at 0.10", err)
	}
	if _, err := holding("0.40"); refusal(err).Dimension != limits.DimensionDailyCost {
		t.Fatalf("got %v for 0.40 on top of 0.10 spent and 0.60 held, want a daily cost refusal", err)
	}
	if err := second.Refund(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := holding("0.40"); err != nil {
		t.Errorf("got %v for 0.40 after the held 0.60 was refunded", err)
	}
}

//go:build integration

package usage

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// factCosts reads the cost columns one event's facts stored.
type factCosts struct {
	estimated  *string
	list       *string
	contracted *string
	caller     bool
}

func persistCosts(t *testing.T, pool *pgxpool.Pool, event *Event) []factCosts {
	t.Helper()
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	result, err := PersistEvent(t.Context(), pool, event, payload)
	if err != nil {
		t.Fatalf("persist event: %v", err)
	}
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	rows, err := pool.Query(t.Context(), `SELECT estimated_cost::text, list_cost::text,
	    contracted_cost::text, caller_credential FROM olp.attempt_usage_facts
	    WHERE request_id = $1::uuid ORDER BY attempt_ordinal`, event.RequestID)
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	defer rows.Close()
	var facts []factCosts
	for rows.Next() {
		var fact factCosts
		if err = rows.Scan(&fact.estimated, &fact.list, &fact.contracted, &fact.caller); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		facts = append(facts, fact)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("read facts: %v", err)
	}
	return facts
}

// seedCostPrices stores one 3-per-million USD price per model in a single
// revision, which is how the pricing selection finds them.
func seedCostPrices(t *testing.T, pool *pgxpool.Pool, owner string, effective time.Time, models ...string) {
	t.Helper()
	cases := make([]parityCase, 0, len(models))
	for _, model := range models {
		cases = append(cases, parityCase{model: model, price: Price{ProviderKind: "openai",
			Model: model, Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}})
	}
	seedParityPrices(t, pool, owner, effective, cases)
}

// TestIntegrationFactCostColumns records the undiscounted list cost and the
// discounted contracted cost beside every stored estimated cost, and neither
// when the attempt is unpriced.
func TestIntegrationFactCostColumns(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	discounted := seedParityProvider(t, pool, owner)
	committed := seedCommittedProvider(t, pool, owner, "300", "day")
	committedDiscounted := seedCommittedProvider(t, pool, owner, "300", "day")
	observed := time.Now().UTC().Add(-time.Minute)
	effective := observed.Add(-time.Hour)
	seedCostPrices(t, pool, owner, effective, "costs-plain", "costs-discounted",
		"costs-committed", "costs-committed-discounted")
	// Discounts are scoped to a connection, so each regime gets its own.
	seedParityDiscount(t, pool, owner, "openai", nil, &discounted, "0.5", effective)
	seedParityDiscount(t, pool, owner, "openai", nil, &committedDiscounted, "0.5", effective)
	complete := func() AttemptUsage {
		return AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}
	}
	// 1000 tokens at 3 per million price at 0.003 list; a 0.5 multiplier
	// contracts it to 0.0015, and a commitment zeroes the estimate only.
	for _, tc := range []struct {
		name       string
		provider   string
		model      string
		estimated  *string
		list       *string
		contracted *string
	}{
		{name: "plain", provider: provider, model: "costs-plain",
			estimated: rate("0.003000000000"), list: rate("0.003000000000"),
			contracted: rate("0.003000000000")},
		{name: "discounted", provider: discounted, model: "costs-discounted",
			estimated: rate("0.001500000000"), list: rate("0.003000000000"),
			contracted: rate("0.001500000000")},
		{name: "committed", provider: committed, model: "costs-committed",
			estimated: rate("0.000000000000"), list: rate("0.003000000000"),
			contracted: rate("0.003000000000")},
		{name: "committed discounted", provider: committedDiscounted, model: "costs-committed-discounted",
			estimated: rate("0.000000000000"), list: rate("0.003000000000"),
			contracted: rate("0.001500000000")},
		{name: "unpriced", provider: provider, model: "costs-unpriced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := parityEvent(t, key, tc.provider, observed, []parityCase{
				{model: tc.model, usage: complete()},
			})
			facts := persistCosts(t, pool, event)
			if len(facts) != 1 {
				t.Fatalf("facts = %d, want 1", len(facts))
			}
			fact := facts[0]
			for _, pair := range [][2]*string{
				{fact.estimated, tc.estimated}, {fact.list, tc.list}, {fact.contracted, tc.contracted},
			} {
				if (pair[0] == nil) != (pair[1] == nil) {
					t.Fatalf("costs = (%v, %v, %v), want (%v, %v, %v)",
						fact.estimated, fact.list, fact.contracted,
						tc.estimated, tc.list, tc.contracted)
				}
				if pair[0] != nil && *pair[0] != *pair[1] {
					t.Fatalf("costs = (%v, %v, %v), want (%v, %v, %v)",
						*fact.estimated, *fact.list, *fact.contracted,
						*tc.estimated, *tc.list, *tc.contracted)
				}
			}
			if fact.caller {
				t.Fatal("operator attempt marked caller-paid")
			}
		})
	}
}

// TestIntegrationCallerCredentialFact marks the attempts the caller's own
// credential served so FinOps exports can exclude them from operator cost.
func TestIntegrationCallerCredentialFact(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	var revision string
	if err := pool.QueryRow(t.Context(), `SELECT active_revision_id::text
	    FROM olp.providers WHERE id=$1::uuid`, provider).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Add(-time.Minute)
	seedCostPrices(t, pool, owner, observed.Add(-time.Hour), "costs-caller-flag")
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "costs-caller-flag", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
		{model: "costs-caller-flag", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	event.Attempts[0].Routing = &Routing{ProviderRevisionID: revision, CredentialSource: "caller"}
	event.Attempts[1].Routing = &Routing{ProviderRevisionID: revision, CredentialSource: "operator"}
	facts := persistCosts(t, pool, event)
	if len(facts) != 2 || !facts[0].caller || facts[1].caller {
		t.Fatalf("caller_credential = %v facts; want the first true only", facts)
	}
	// Caller-paid usage is still priced and accounted.
	if facts[0].list == nil || *facts[0].list != "0.003000000000" {
		t.Fatalf("caller list = %v", facts[0].list)
	}
}

// TestIntegrationCostPairConstraint holds the coupling: both costs or neither,
// and never without an estimated cost.
func TestIntegrationCostPairConstraint(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedCostPrices(t, pool, owner, observed.Add(-time.Hour), "costs-pair")
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "costs-pair", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	persistCosts(t, pool, event)
	for _, change := range []string{
		"list_cost = NULL",
		"contracted_cost = NULL",
		"list_cost = -1",
		"contracted_cost = -1",
		"list_cost = 1, estimated_cost = NULL",
	} {
		if _, err := pool.Exec(t.Context(), `UPDATE olp.attempt_usage_facts SET `+change+
			` WHERE request_id=$1::uuid`, event.RequestID); err == nil {
			t.Fatalf("%s was accepted", change)
		}
	}
}

// TestIntegrationHourlyCostRollup sums list and contracted costs across a
// rollup, resolving the pre-0067 spelling of a priced fact — NULL costs beside
// an estimated cost — as estimated_cost + commitment_weight on both sides of a
// merge.
func TestIntegrationHourlyCostRollup(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	committed := seedCommittedProvider(t, pool, owner, "300", "day")
	observed := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour).Add(30 * time.Minute)
	seedCostPrices(t, pool, owner, observed.Add(-time.Hour), "costs-hourly")
	cutoff := observed.Truncate(time.Hour).Add(time.Hour)
	complete := func(count int64) AttemptUsage {
		return AttemptUsage{Observed: true, Complete: true, InputTokens: &count}
	}
	roll := func() int64 {
		t.Helper()
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		_, facts, err := rollUpAttemptUsage(t.Context(), conn.Conn(), cutoff)
		if err != nil {
			t.Fatalf("rollup: %v", err)
		}
		return facts
	}
	hourly := func(providerID, model string) (estimated, list, contracted *string) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT estimated_cost::text, list_cost::text,
		    contracted_cost::text FROM olp.attempt_usage_hourly
		    WHERE provider_id=$1::uuid AND upstream_model=$2`, providerID, model).
			Scan(&estimated, &list, &contracted); err != nil {
			t.Fatalf("read hourly %s: %v", model, err)
		}
		return
	}
	persist := func(providerID, model string, count int64) *Event {
		t.Helper()
		ev := parityEvent(t, key, providerID, observed, []parityCase{
			{model: model, usage: complete(count)},
		})
		persistCosts(t, pool, ev)
		return ev
	}
	// staleCost returns a fact to its pre-0067 spelling: priced, with no list
	// or contracted cost recorded.
	staleCost := func(event *Event) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `UPDATE olp.attempt_usage_facts
		    SET list_cost=NULL, contracted_cost=NULL WHERE event_id=$1::uuid`, event.EventID); err != nil {
			t.Fatal(err)
		}
	}

	// Two new-format facts roll into one row: 0.003 + 0.006 each way.
	persist(provider, "costs-hourly", 1000)
	persist(provider, "costs-hourly", 2000)
	if roll() != 2 {
		t.Fatal("first rollup did not fold both facts")
	}
	if estimated, list, contracted := hourly(provider, "costs-hourly"); *estimated != "0.009000000000" ||
		*list != "0.009000000000" || *contracted != "0.009000000000" {
		t.Fatalf("hourly = (%v, %v, %v), want all 0.009", *estimated, *list, *contracted)
	}

	// A pre-0067 fact resolves to its estimated cost on the way in.
	staleCost(persist(provider, "costs-hourly", 1000))
	if roll() != 1 {
		t.Fatal("second rollup did not fold the pre-0067 fact")
	}
	if estimated, list, contracted := hourly(provider, "costs-hourly"); *estimated != "0.012000000000" ||
		*list != "0.012000000000" || *contracted != "0.012000000000" {
		t.Fatalf("hourly = (%v, %v, %v), want all 0.012", *estimated, *list, *contracted)
	}

	// A pre-0067 hourly row (NULL costs beside a real estimate) resolves to
	// estimated + weight before the new contribution is added.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.attempt_usage_hourly
	    SET list_cost=NULL, contracted_cost=NULL
	    WHERE provider_id=$1::uuid AND upstream_model='costs-hourly'`, provider); err != nil {
		t.Fatal(err)
	}
	persist(provider, "costs-hourly", 1000)
	if roll() != 1 {
		t.Fatal("third rollup did not fold the merge fact")
	}
	if estimated, list, contracted := hourly(provider, "costs-hourly"); *estimated != "0.015000000000" ||
		*list != "0.015000000000" || *contracted != "0.015000000000" {
		t.Fatalf("hourly = (%v, %v, %v), want all 0.015", *estimated, *list, *contracted)
	}

	// A pre-0067 committed fact resolves both costs to its commitment weight:
	// the estimate was already zeroed, so the weight is all that remains.
	staleCost(persist(committed, "costs-hourly", 1000))
	if roll() != 1 {
		t.Fatal("fourth rollup did not fold the committed fact")
	}
	if estimated, list, contracted := hourly(committed, "costs-hourly"); *estimated != "0.000000000000" ||
		*list != "0.003000000000" || *contracted != "0.003000000000" {
		t.Fatalf("committed hourly = (%v, %v, %v), want 0, 0.003, 0.003", *estimated, *list, *contracted)
	}

	// An unpriced rollup leaves every cost NULL.
	persist(provider, "costs-hourly-noprice", 1000)
	if roll() != 1 {
		t.Fatal("fifth rollup did not fold the unpriced fact")
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_hourly
	    WHERE upstream_model='costs-hourly-noprice'
	      AND estimated_cost IS NULL AND list_cost IS NULL AND contracted_cost IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("unpriced hourly rows with NULL costs = %d, want 1", n)
	}
}

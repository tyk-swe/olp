//go:build integration

package usage

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// parityCase is one price and one attempt's usage to bill against it.
type parityCase struct {
	model string
	price Price
	usage AttemptUsage
}

// TestIntegrationPricingParity holds Go pricing to the SQL that accounts for
// every attempt. The gateway reserves a request's cost and settles it with
// Price.Cost, and the budget counts that amount until PostgreSQL's own figure
// replaces it; the two must agree to the last unit, including where the numeric
// division picks its own scale and where the SQL records no cost at all. It runs
// the real priceAttemptSQL, then the same attempts through PersistEventTx so
// the numeric(24,12) rounding on insert and the spend delta are checked too.
func TestIntegrationPricingParity(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	cases := parityCases(2400)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), cases)

	t.Run("priceAttemptSQL", func(t *testing.T) {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		event := &Event{Operation: "generation", ObservedAt: observed}
		var pricedCases, unpricedCases int
		for index, c := range cases {
			usage := c.usage
			attempt := ValidatedAttempt{Attempt: &Attempt{ProviderID: provider, UpstreamModel: c.model}, Usage: usage}
			got, err := priceAttempt(t.Context(), tx, event, attempt)
			if err != nil {
				t.Fatalf("case %d: %v", index, err)
			}
			want, ok := c.price.Cost(usage)
			rated := parseRates(&c.price).rated(usage)
			if got.complete != rated {
				t.Fatalf("case %d: SQL complete = %v, Go rated = %v\nprice %s\nusage %s", index, got.complete, rated, describePrice(c.price), describe(usage))
			}
			if (got.estimatedCost != nil) != ok {
				t.Fatalf("case %d: SQL cost present = %v, Go priced = %v\nprice %s\nusage %s", index, got.estimatedCost != nil, ok, describePrice(c.price), describe(usage))
			}
			if !ok {
				unpricedCases++
				continue
			}
			pricedCases++
			// The SQL returns the sum unrounded; the column the fact lands in
			// rounds it to twelve places.
			var stored string
			if err = tx.QueryRow(t.Context(), `SELECT $1::text::numeric(24,12)::text`, *got.estimatedCost).Scan(&stored); err != nil {
				t.Fatalf("case %d: round %q: %v", index, *got.estimatedCost, err)
			}
			if sqlCost := parityCost(t, stored); sqlCost.Cmp(want) != 0 {
				t.Fatalf("case %d: SQL cost %s, Go cost %s\nprice %s\nusage %s", index, stored, want, describePrice(c.price), describe(usage))
			}
		}
		if pricedCases < len(cases)/2 || unpricedCases < len(cases)/20 {
			t.Fatalf("the generator exercised %d priced and %d unpriced cases, want both well covered", pricedCases, unpricedCases)
		}
	})

	// An event that names cached tokens without their input is refused when it is
	// validated, so only the SQL above ever sees one.
	var accountable []parityCase
	for _, c := range cases {
		if c.usage.InputTokens != nil || c.usage.CachedInputTokens == nil {
			accountable = append(accountable, c)
		}
	}

	t.Run("PersistEventTx", func(t *testing.T) {
		for index := 0; index < len(accountable); index += 12 {
			c := accountable[index]
			event := parityEvent(t, key, provider, observed, []parityCase{c})
			result, stored := persistParity(t, pool, event)
			if result.Outcome != PersistOutcomePersisted {
				t.Fatalf("case %d: outcome %v", index, result.Outcome)
			}
			want, ok := c.price.Cost(c.usage)
			if (stored[0].cost != nil) != ok {
				t.Fatalf("case %d: stored cost present = %v, Go priced = %v", index, stored[0].cost != nil, ok)
			}
			if ok {
				if got := parityCost(t, *stored[0].cost); got.Cmp(want) != 0 {
					t.Fatalf("case %d: stored cost %s, Go cost %s\nprice %s\nusage %s", index, *stored[0].cost, want, describePrice(c.price), describe(c.usage))
				}
			}
		}
	})

	t.Run("a request's spend is the sum of what each attempt costs", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(5, 9))
		for event := range 60 {
			// A request that failed over bills each attempt at its own price and
			// rounds each on its own, so the delta is the sum of rounded amounts.
			attempts := make([]parityCase, 1+rng.IntN(3))
			var want Cost
			for index := range attempts {
				attempts[index] = accountable[rng.IntN(len(accountable))]
				if cost, ok := attempts[index].price.Cost(attempts[index].usage); ok {
					want = want.Add(cost)
				}
			}
			// A key of its own starts from nothing, so its first delta is the total.
			own := seedParityKey(t, pool, owner)
			accounted := parityEvent(t, own, provider, observed, attempts)
			result, _ := persistParity(t, pool, accounted)
			if len(result.CostSnapshots) != 1 {
				t.Fatalf("event %d: %d snapshots, want one", event, len(result.CostSnapshots))
			}
			snapshot := result.CostSnapshots[0]
			if got := parityCost(t, snapshot.DailyAccrued); got.Cmp(want) != 0 {
				t.Fatalf("event %d: spend delta %s, sum of Go costs %s", event, snapshot.DailyAccrued, want)
			}
			if snapshot.RequestID != accounted.RequestID {
				t.Fatalf("event %d: the snapshot names request %q, want %q, whose reservation it releases", event, snapshot.RequestID, accounted.RequestID)
			}
		}
	})
}

func describePrice(p Price) string {
	var parts []string
	for _, field := range [...]struct {
		name  string
		value *string
	}{
		{"input", p.InputPerMillion}, {"output", p.OutputPerMillion}, {"cached", p.CachedInputPerMillion},
		{"write", p.CacheWriteInputPerMillion}, {"write5m", p.CacheWrite5MInputPerMillion},
		{"write1h", p.CacheWrite1HInputPerMillion}, {"unit", p.UnitPrice},
	} {
		if field.value != nil {
			parts = append(parts, field.name+"="+*field.value)
		}
	}
	return strings.Join(parts, " ")
}

func describe(u AttemptUsage) string {
	var parts []string
	for _, field := range [...]struct {
		name  string
		value *int64
	}{
		{"input", u.InputTokens}, {"output", u.OutputTokens}, {"cached", u.CachedInputTokens},
		{"write", u.CacheWriteInputTokens}, {"write5m", u.CacheWrite5MInputTokens},
		{"write1h", u.CacheWrite1HInputTokens},
	} {
		if field.value != nil {
			parts = append(parts, fmt.Sprintf("%s=%d", field.name, *field.value))
		}
	}
	if u.MediaUnits != nil {
		parts = append(parts, "media="+*u.MediaUnits)
	}
	return fmt.Sprintf("complete=%v %s", u.Complete, strings.Join(parts, " "))
}

// parityCost reads a numeric(24,12) the way the fixtures do.
func parityCost(t *testing.T, text string) Cost {
	t.Helper()
	units, ok := scaledDecimal(text, costDigits)
	if !ok {
		t.Fatalf("%q is not a cost", text)
	}
	return Cost{units}
}

// parityRate is a rate of the given magnitude with twelve fractional digits, the
// last ones often zero as a hand-written price is.
func parityRate(rng *rand.Rand, magnitude int) *string {
	var digits strings.Builder
	for index := range 1 + rng.IntN(magnitude) {
		digit := rng.IntN(10)
		if index == 0 && digit == 0 {
			digit = 1
		}
		digits.WriteByte(byte('0' + digit))
	}
	digits.WriteByte('.')
	kept := rng.IntN(13)
	for index := range 12 {
		if index < kept {
			digits.WriteByte(byte('0' + rng.IntN(10)))
		} else {
			digits.WriteByte('0')
		}
	}
	text := digits.String()
	return &text
}

// parityCases generates prices and usage across the regimes the SQL treats
// differently: rates from a fraction of a cent to billions per million, so the
// numerator's weight and the quotient scale vary, sparse tiers that fall back to
// the next rate, counts up to two billion tokens, media units, and the
// combinations that leave no cost.
func parityCases(n int) []parityCase {
	rng := rand.New(rand.NewPCG(42, 2024))
	count := func(limit int64) *int64 { value := rng.Int64N(limit + 1); return &value }
	cases := make([]parityCase, 0, n)
	for len(cases) < n {
		magnitude := []int{1, 2, 3, 5, 8, 11}[rng.IntN(6)]
		price := Price{ProviderKind: "openai", Model: fmt.Sprintf("parity-%d", len(cases)), Operation: "generation",
			InputPerMillion: parityRate(rng, magnitude)}
		sometimes := func(oneIn int) *string {
			if rng.IntN(oneIn) == 0 {
				return parityRate(rng, magnitude)
			}
			return nil
		}
		if rng.IntN(5) > 0 {
			price.OutputPerMillion = parityRate(rng, magnitude)
		}
		price.CachedInputPerMillion, price.CacheWriteInputPerMillion = sometimes(2), sometimes(2)
		price.CacheWrite5MInputPerMillion, price.CacheWrite1HInputPerMillion = sometimes(3), sometimes(3)
		price.UnitPrice = sometimes(4)
		limit := []int64{10, 1000, 100_000, 10_000_000, 2_000_000_000}[rng.IntN(5)]
		usage := AttemptUsage{Observed: true, Complete: rng.IntN(12) > 0}
		input := rng.Int64N(limit + 1)
		usage.InputTokens = &input
		remaining := input
		if rng.IntN(2) == 0 {
			usage.CachedInputTokens = count(remaining)
			remaining -= *usage.CachedInputTokens
		}
		if rng.IntN(2) == 0 {
			usage.CacheWriteInputTokens = count(remaining)
			if rng.IntN(2) == 0 {
				usage.CacheWrite5MInputTokens = count(*usage.CacheWriteInputTokens)
			}
			if rng.IntN(2) == 0 {
				split := *usage.CacheWriteInputTokens
				if usage.CacheWrite5MInputTokens != nil {
					split -= *usage.CacheWrite5MInputTokens
				}
				usage.CacheWrite1HInputTokens = count(split)
			}
		}
		if rng.IntN(5) > 0 {
			usage.OutputTokens = count(limit)
		}
		if rng.IntN(8) == 0 {
			media := fmt.Sprintf("%d.%03d", rng.IntN(1000), rng.IntN(1000))
			usage.MediaUnits = &media
		}
		switch rng.IntN(20) {
		case 0:
			// Cached tokens that never said what they were cached out of.
			usage.InputTokens, usage.CacheWriteInputTokens = nil, nil
			usage.CacheWrite5MInputTokens, usage.CacheWrite1HInputTokens = nil, nil
			if usage.CachedInputTokens == nil {
				usage.CachedInputTokens = count(10)
			}
		case 1:
			usage.InputTokens, usage.CachedInputTokens, usage.CacheWriteInputTokens = nil, nil, nil
			usage.CacheWrite5MInputTokens, usage.CacheWrite1HInputTokens = nil, nil
		}
		if cost, ok := price.Cost(usage); ok && cost.Cmp(MaxCost()) >= 0 {
			// PostgreSQL refuses an amount the column cannot hold; admission
			// saturates instead, and that is not what is compared here.
			continue
		}
		cases = append(cases, parityCase{model: price.Model, price: price, usage: usage})
	}
	return cases
}

func seedParityOwner(t *testing.T, pool *pgxpool.Pool) (owner, key string) {
	t.Helper()
	owner = uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users (id, email, display_name, role, etag)
	    VALUES ($1::uuid, $2, 'Parity Owner', 'owner', $3::uuid)`,
		owner, "parity-"+owner+"@example.test", uuid.NewString()); err != nil {
		t.Fatalf("seed parity owner: %v", err)
	}
	return owner, seedParityKey(t, pool, owner)
}

// seedParityKey adds an API key whose spend starts from nothing.
func seedParityKey(t *testing.T, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	key := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.api_keys (id, lookup_id, digest, name, created_by, policy, etag)
	    VALUES ($1::uuid, $2, $3, 'parity', $4::uuid, '{}'::jsonb, $5::uuid)`,
		key, "lookup-"+key, []byte("digest"), owner, uuid.NewString()); err != nil {
		t.Fatalf("seed parity key: %v", err)
	}
	return key
}

func seedParityProvider(t *testing.T, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	provider, revision := uuid.NewString(), uuid.NewString()
	const configuration = `{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{"vendor_id":"openai"}}`
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers
	        (id, name, kind, state, configuration, etag, slots_etag, active_revision, active_revision_id, created_by)
	    VALUES ($1::uuid, $2, 'openai', 'active', $3::jsonb, $4::uuid, $5::uuid, 1, $6::uuid, $7::uuid)`,
		provider, "provider-"+provider, configuration, uuid.NewString(), uuid.NewString(), revision, owner); err != nil {
		t.Fatalf("seed parity provider: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_revisions
	        (id, provider_id, revision, name, configuration, models, slots, source_etag, activated_by)
	    VALUES ($1::uuid, $2::uuid, 1, $3, $4::jsonb, '[]'::jsonb, '[]'::jsonb, $5::uuid, $6::uuid)`,
		revision, provider, "provider-"+provider, configuration, uuid.NewString(), owner); err != nil {
		t.Fatalf("seed parity provider revision: %v", err)
	}
	return provider
}

// seedParityPrices stores one price per case in a single revision, which is how
// the SQL under test finds them: by provider kind, model and operation.
func seedParityPrices(t *testing.T, pool *pgxpool.Pool, owner string, effective time.Time, cases []parityCase) {
	t.Helper()
	revision := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.pricing_revisions (id, revision, effective_at, created_by)
	    VALUES ($1::uuid, 1, $2, $3::uuid)`, revision, effective, owner); err != nil {
		t.Fatalf("seed pricing revision: %v", err)
	}
	batch := &pgx.Batch{}
	for _, c := range cases {
		p := c.price
		batch.Queue(`INSERT INTO olp.prices
		        (pricing_revision_id, provider_kind, model, operation, input_per_million, output_per_million,
		         cached_input_per_million, cache_write_input_per_million, cache_write_5m_input_per_million,
		         cache_write_1h_input_per_million, unit_price, currency)
		    VALUES ($1::uuid, 'openai', $2, 'generation', $3::text::numeric, $4::text::numeric, $5::text::numeric,
		            $6::text::numeric, $7::text::numeric, $8::text::numeric, $9::text::numeric, 'USD')`,
			revision, p.Model, p.InputPerMillion, p.OutputPerMillion, p.CachedInputPerMillion,
			p.CacheWriteInputPerMillion, p.CacheWrite5MInputPerMillion, p.CacheWrite1HInputPerMillion, p.UnitPrice)
	}
	results := pool.SendBatch(t.Context(), batch)
	for range cases {
		if _, err := results.Exec(); err != nil {
			results.Close()
			t.Fatalf("seed prices: %v", err)
		}
	}
	if err := results.Close(); err != nil {
		t.Fatalf("seed prices: %v", err)
	}
}

// parityEvent is a request whose attempts each carry one case's usage.
func parityEvent(t *testing.T, key, provider string, observed time.Time, attempts []parityCase) *Event {
	t.Helper()
	status := 200
	event := &Event{
		Version: WireVersion, EventID: uuid.NewString(), RequestID: uuid.NewString(),
		RuntimeGenerationID: uuid.NewString(), APIKeyID: key, RouteSlug: "parity",
		Operation: "generation", Surface: "openai",
		RequestStartedAt: observed.Add(-time.Second), RequestCompletedAt: observed,
		ObservedAt: observed, StatusCode: &status, LatencyMS: 1000,
	}
	for index, c := range attempts {
		usage := c.usage
		started := observed.Add(-time.Second)
		event.Attempts = append(event.Attempts, Attempt{
			ID: uuid.NewString(), Ordinal: index + 1, ProviderID: provider, UpstreamModel: c.model,
			StartedAt: started, CompletedAt: started.Add(500 * time.Millisecond),
			StatusCode: &status, Committed: true, LatencyMS: 500, Usage: &usage,
		})
	}
	final := event.Attempts[len(event.Attempts)-1]
	event.ProviderID, event.UpstreamModel, event.Committed = &final.ProviderID, &final.UpstreamModel, final.Committed
	return event
}

type parityFact struct{ cost *string }

// persistParity accounts for an event as the consumer would and reads back what
// each attempt's fact stored.
func persistParity(t *testing.T, pool *pgxpool.Pool, event *Event) (Persisted, []parityFact) {
	t.Helper()
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	result, err := PersistEvent(t.Context(), pool, event, payload)
	if err != nil {
		t.Fatalf("persist event: %v", err)
	}
	rows, err := pool.Query(t.Context(), `SELECT estimated_cost::text FROM olp.attempt_usage_facts
	    WHERE request_id = $1::uuid ORDER BY attempt_ordinal`, event.RequestID)
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	defer rows.Close()
	var facts []parityFact
	for rows.Next() {
		var fact parityFact
		if err = rows.Scan(&fact.cost); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		facts = append(facts, fact)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("read facts: %v", err)
	}
	return result, facts
}

//go:build integration

package usage

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

// seedCommittedProvider stores a provider whose revision carries a
// provisioned-capacity commitment in its options.
func seedCommittedProvider(t *testing.T, pool *pgxpool.Pool, owner, amount, period string) string {
	t.Helper()
	provider, revision := uuid.NewString(), uuid.NewString()
	configuration := `{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{"commitment":{"amount":"` + amount + `","period":"` + period + `"}}}`
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers
	        (id, name, kind, state, configuration, etag, slots_etag, active_revision, active_revision_id, created_by)
	    VALUES ($1::uuid, $2, 'openai', 'active', $3::jsonb, $4::uuid, $5::uuid, 1, $6::uuid, $7::uuid)`,
		provider, "provider-"+provider, configuration, uuid.NewString(), uuid.NewString(), revision, owner); err != nil {
		t.Fatalf("seed committed provider: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_revisions
	        (id, provider_id, revision, name, configuration, models, slots, source_etag, activated_by)
	    VALUES ($1::uuid, $2::uuid, 1, $3, $4::jsonb, '[]'::jsonb, '[]'::jsonb, $5::uuid, $6::uuid)`,
		revision, provider, "provider-"+provider, configuration, uuid.NewString(), owner); err != nil {
		t.Fatalf("seed committed provider revision: %v", err)
	}
	return provider
}

// TestIntegrationCommitmentAccounting prices a committed attempt at zero with
// its list cost recorded as the weight the commitment divides by, while a
// usage the price cannot price stays unpriced with no weight.
func TestIntegrationCommitmentAccounting(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedCommittedProvider(t, pool, owner, "300", "day")
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "committed-model", price: Price{ProviderKind: "openai", Model: "committed-model",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "committed-model", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
		{model: "unpriced-model", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(500)}},
	})
	result, facts := persistParity(t, pool, event)
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	if facts[0].cost == nil || *facts[0].cost != "0.000000000000" {
		t.Fatalf("committed cost = %v, want 0", facts[0].cost)
	}
	if facts[1].cost != nil {
		t.Fatalf("unpriced attempt gained a cost: %v", *facts[1].cost)
	}
	var weight, amount, period *string
	var components []string
	if err := pool.QueryRow(t.Context(), `SELECT commitment_weight::text, commitment_amount::text,
	    commitment_period, pricing_components FROM olp.attempt_usage_facts
	    WHERE event_id=$1::uuid AND attempt_ordinal=1`, event.EventID).
		Scan(&weight, &amount, &period, &components); err != nil {
		t.Fatalf("read commitment columns: %v", err)
	}
	// 1000 tokens at 3 per million is a 0.003 list cost weight.
	if weight == nil || *weight != "0.003000000000" || amount == nil || *amount != "300.000000000000" ||
		period == nil || *period != "day" {
		t.Fatalf("commitment = %v %v %v", weight, amount, period)
	}
	found := false
	for _, c := range components {
		found = found || c == "commitment"
	}
	if !found {
		t.Fatalf("components = %v, want commitment", components)
	}
	var unpricedWeight *string
	if err := pool.QueryRow(t.Context(), `SELECT commitment_weight::text FROM olp.attempt_usage_facts
	    WHERE event_id=$1::uuid AND attempt_ordinal=2`, event.EventID).Scan(&unpricedWeight); err != nil {
		t.Fatalf("read unpriced weight: %v", err)
	}
	if unpricedWeight != nil {
		t.Fatalf("unpriced attempt gained weight: %v", *unpricedWeight)
	}
}

// TestIntegrationCommitmentAllocation divides one period's amount across the
// two models' list-cost weights, and the split survives facts folding into
// the hourly rollup.
func TestIntegrationCommitmentAllocation(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedCommittedProvider(t, pool, owner, "300", "hour")
	// Both attempts sit in the same UTC hour so the day's worth of weight is
	// the two list costs. The period is whole inside the report range, so the
	// allocations sum to the amount.
	observed := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour).Add(30 * time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "model-a", price: Price{ProviderKind: "openai", Model: "model-a",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
		{model: "model-b", price: Price{ProviderKind: "openai", Model: "model-b",
			Operation: "generation", InputPerMillion: rate("6"), Currency: "USD"}},
	})
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "model-a", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
		{model: "model-b", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(2000)}},
	})
	result, _ := persistParity(t, pool, event)
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	// Weights are 0.003 and 0.012, so model-a gets 300*0.2=60, model-b 240.
	filters := Filters{Start: observed.Truncate(time.Hour), End: observed.Truncate(time.Hour).Add(time.Hour), AllProjects: true}
	summary, err := ReadSummary(t.Context(), pool, filters, time.Now())
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if summary.AllocatedCommitment != "300.000000000000" {
		t.Fatalf("allocated = %s, want the full 300", summary.AllocatedCommitment)
	}
	breakdown, err := ReadBreakdown(t.Context(), pool, filters, DimensionModel, 10)
	if err != nil {
		t.Fatalf("read breakdown: %v", err)
	}
	got := map[string]string{}
	for _, item := range breakdown.Items {
		got[item.Dimension] = item.AllocatedCommitment
	}
	if got["model-a"] != "60.000000000000" || got["model-b"] != "240.000000000000" {
		t.Fatalf("model allocations = %v, want 60/240", got)
	}
	// A filtered report still divides by the whole period's weight.
	only := "model-a"
	filtered, err := ReadSummary(t.Context(), pool, Filters{Start: filters.Start, End: filters.End, Model: &only, AllProjects: true}, time.Now())
	if err != nil {
		t.Fatalf("read filtered summary: %v", err)
	}
	if filtered.AllocatedCommitment != "60.000000000000" {
		t.Fatalf("filtered allocation = %s, want 60", filtered.AllocatedCommitment)
	}

	// The rollup keeps the weight and dimensions, so a report whose range
	// covers only whole buckets allocates identically from hourly rows.
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, facts, err := rollUpAttemptUsage(t.Context(), conn.Conn(), observed.Truncate(time.Hour).Add(time.Hour))
	conn.Release()
	if err != nil || facts != 2 {
		t.Fatalf("rollup folded %d facts: %v", facts, err)
	}
	var hourlyWeight string
	var hourlyRows int
	if err := pool.QueryRow(t.Context(), `SELECT SUM(commitment_weight)::text, COUNT(*)
	    FROM olp.attempt_usage_hourly WHERE provider_id=$1::uuid`, provider).
		Scan(&hourlyWeight, &hourlyRows); err != nil {
		t.Fatalf("read hourly commitment: %v", err)
	}
	if hourlyWeight != "0.015000000000" || hourlyRows != 2 {
		t.Fatalf("hourly commitment = %s over %d rows", hourlyWeight, hourlyRows)
	}
	summary, err = ReadSummary(t.Context(), pool, filters, time.Now())
	if err != nil {
		t.Fatalf("read rolled-up summary: %v", err)
	}
	if summary.AllocatedCommitment != "300.000000000000" {
		t.Fatalf("rolled-up allocation = %s, want 300", summary.AllocatedCommitment)
	}
}

// TestIntegrationNullCommitmentIsNotCommitted proves a stored
// `"commitment": null` — the column keeps native JSON, so it is JSON null, not
// SQL NULL — prices the attempt normally rather than violating the fact
// constraint a NULL commitment would produce.
func TestIntegrationNullCommitmentIsNotCommitted(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	seedParityPrices(t, pool, owner, time.Now().UTC().Add(-time.Hour), []parityCase{
		{model: "plain-model", price: Price{ProviderKind: "openai", Model: "plain-model",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	for _, tc := range []struct {
		name          string
		commitmentKey string
	}{
		{name: "absent", commitmentKey: `"vendor_id":"openai"`},
		{name: "null", commitmentKey: `"vendor_id":"openai","commitment":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, revision := uuid.NewString(), uuid.NewString()
			configuration := `{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{` + tc.commitmentKey + `}}`
			if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers
			    (id, name, kind, state, configuration, etag, slots_etag, active_revision, active_revision_id, created_by)
			    VALUES ($1::uuid, $2, 'openai', 'active', $3::jsonb, $4::uuid, $5::uuid, 1, $6::uuid, $7::uuid)`,
				provider, "provider-"+provider, configuration, uuid.NewString(), uuid.NewString(), revision, owner); err != nil {
				t.Fatalf("seed provider: %v", err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_revisions
			    (id, provider_id, revision, name, configuration, models, slots, source_etag, activated_by)
			    VALUES ($1::uuid, $2::uuid, 1, $3, $4::jsonb, '[]'::jsonb, '[]'::jsonb, $5::uuid, $6::uuid)`,
				revision, provider, "provider-"+provider, configuration, uuid.NewString(), owner); err != nil {
				t.Fatalf("seed provider revision: %v", err)
			}
			observed := time.Now().UTC().Add(-time.Minute)
			event := parityEvent(t, key, provider, observed, []parityCase{
				{model: "plain-model", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
			})
			result, facts := persistParity(t, pool, event)
			if result.Outcome != PersistOutcomePersisted {
				t.Fatalf("outcome = %v", result.Outcome)
			}
			if facts[0].cost == nil || *facts[0].cost != "0.003000000000" {
				t.Fatalf("cost = %v, want 0.003000000000", facts[0].cost)
			}
			var weight *string
			if err := pool.QueryRow(t.Context(), `SELECT commitment_weight::text FROM olp.attempt_usage_facts
			    WHERE event_id=$1::uuid`, event.EventID).Scan(&weight); err != nil {
				t.Fatalf("read weight: %v", err)
			}
			if weight != nil {
				t.Fatalf("weight = %v", *weight)
			}
		})
	}
}

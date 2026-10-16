//go:build integration

package usage

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

// 1000 tokens at 3 per million is a 0.003 list cost weight.

// Both attempts sit in the same UTC hour so the day's worth of weight is
// the two list costs. The period is whole inside the report range, so the
// allocations sum to the amount.

// Weights are 0.003 and 0.012, so model-a gets 300*0.2=60, model-b 240.

// A filtered report still divides by the whole period's weight.

// The rollup keeps the weight and dimensions, so a report whose range
// covers only whole buckets allocates identically from hourly rows.

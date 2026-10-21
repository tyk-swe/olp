//go:build integration

package usage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedParityDiscount stores one discount at the given scope.
func seedParityDiscount(t *testing.T, pool *pgxpool.Pool, owner, kind string,
	vendorID, providerID *string, multiplier string, effective time.Time) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.pricing_discounts
	    (id, provider_kind, vendor_id, provider_id, multiplier, effective_at, created_by)
	    VALUES ($1::uuid, $2, $3, $4::uuid, $5::text::numeric, $6, $7::uuid)`,
		id, kind, vendorID, providerID, multiplier, effective, owner); err != nil {
		t.Fatalf("seed discount: %v", err)
	}
	return id
}

// seedParityProjectKey gives the owner an organization, a project in it and an
// API key owned by the project, which is the consumer projects charge to.
func seedParityProjectKey(t *testing.T, pool *pgxpool.Pool, owner string) (organization, project, key string) {
	t.Helper()
	organization, project, key = uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.organizations (id, name, etag, created_by)
	    VALUES ($1::uuid, $2, $3::uuid, $4::uuid)`,
		organization, "org-"+organization, uuid.NewString(), owner); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects (id, organization_id, name, etag, created_by)
	    VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::uuid)`,
		project, organization, "project-"+project, uuid.NewString(), owner); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.api_keys
	    (id, lookup_id, digest, name, project_id, created_by, policy, etag)
	    VALUES ($1::uuid, $2, $3, 'parity', $4::uuid, $5::uuid, '{}'::jsonb, $6::uuid)`,
		key, "lookup-"+key, []byte("digest"), project, owner, uuid.NewString()); err != nil {
		t.Fatalf("seed project key: %v", err)
	}
	return organization, project, key
}

// seedParityCard stores a card for the owner and one revision in force.
func seedParityCard(t *testing.T, pool *pgxpool.Pool, owner string, organizationID, projectID *string,
	markup string, effective time.Time) (cardID, revisionID string) {
	t.Helper()
	cardID, revisionID = uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_cards
	    (id, organization_id, project_id, created_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid)`,
		cardID, organizationID, projectID, owner); err != nil {
		t.Fatalf("seed rate card: %v", err)
	}
	seedParityCardRevision(t, pool, owner, cardID, revisionID, 1, markup, effective)
	return cardID, revisionID
}

// seedParityCardRevision stores one revision; its explicit prices can be added
// with seedParityCardPrice.
func seedParityCardRevision(t *testing.T, pool *pgxpool.Pool, owner, cardID, revisionID string,
	revision int, markup string, effective time.Time) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_card_revisions
	    (id, rate_card_id, revision, markup, effective_at, created_by)
	    VALUES ($1::uuid, $2::uuid, $3, $4::text::numeric, $5, $6::uuid)`,
		revisionID, cardID, revision, markup, effective, owner); err != nil {
		t.Fatalf("seed card revision: %v", err)
	}
}

// seedParityCardPrice stores one explicit rate on a card revision.
func seedParityCardPrice(t *testing.T, pool *pgxpool.Pool, revisionID string, price Price) {
	t.Helper()
	var toolCalls, tiers, windows any
	if len(price.ToolCalls) > 0 {
		data, _ := json.Marshal(price.ToolCalls)
		toolCalls = string(data)
	}
	if len(price.Tiers) > 0 {
		data, _ := json.Marshal(price.Tiers)
		tiers = string(data)
	}
	if price.Windows != nil {
		data, _ := json.Marshal(price.Windows)
		windows = string(data)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_card_prices
	    (rate_card_revision_id, provider_kind, vendor_id, model, operation,
	     input_per_million, output_per_million, cached_input_per_million,
	     cache_write_input_per_million, cache_write_5m_input_per_million, cache_write_1h_input_per_million,
	     audio_input_per_million, cached_audio_input_per_million, audio_output_per_million,
	     image_input_per_million, cached_image_input_per_million, image_output_per_million,
	     unit_price, per_request, batch_multiplier, tool_calls, tiers, windows, currency)
	    VALUES ($1::uuid, $2, $3, $4, $5, $6::text::numeric, $7::text::numeric, $8::text::numeric,
	            $9::text::numeric, $10::text::numeric, $11::text::numeric, $12::text::numeric,
	            $13::text::numeric, $14::text::numeric, $15::text::numeric, $16::text::numeric,
	            $17::text::numeric, $18::text::numeric, $19::text::numeric, $20::text::numeric,
	            $21::jsonb, $22::jsonb, $23::jsonb, $24)`,
		revisionID, price.ProviderKind, price.VendorID, price.Model, price.Operation,
		price.InputPerMillion, price.OutputPerMillion, price.CachedInputPerMillion,
		price.CacheWriteInputPerMillion, price.CacheWrite5MInputPerMillion, price.CacheWrite1HInputPerMillion,
		price.AudioInputPerMillion, price.CachedAudioInputPerMillion, price.AudioOutputPerMillion,
		price.ImageInputPerMillion, price.CachedImageInputPerMillion, price.ImageOutputPerMillion,
		price.UnitPrice, price.PerRequest, price.BatchMultiplier, toolCalls, tiers, windows, price.Currency); err != nil {
		t.Fatalf("seed card price: %v", err)
	}
}

// Seed specific scopes first and the general one last so insertion order
// cannot decide the answer.

// list cost 1000 x 3 / 1e6 = 0.003, provider discount 0.1 -> 0.0003.

// With no card, charge equals cost.

// A scheduled multiplier must not apply yet.

// The scheduled discount honoured exactly despite not being in force.

// A pinned null discounts nothing even though one is in force.

// No pin: the observed-time selection wins.

// A newer in-force revision replaces the card's own terms wholly.

// A scheduled revision must not apply.

// cost 0.003 x project card's newest in-force markup 5.

// No provider price covers this model: cost NULL, charge from the card.

// Mirror: same usage, window 1.5, batch 0.3, discount 0.5.

// 0.003 x pinned installation markup 2, not the project card's 4.

// The project card only schedules a revision: the organization card wins.

// cost 0.003 x organization markup 3; a future-only project card cannot win.

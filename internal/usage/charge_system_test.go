//go:build integration

package usage

import (
	"encoding/json"
	"strings"
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

type chargedFact struct {
	cost      *string
	charge    *string
	discount  *string
	cardRev   *string
	uncharged bool
}

// persistCharge accounts for an event and reads each fact's cost and charge.
func persistCharge(t *testing.T, pool *pgxpool.Pool, event *Event) (Persisted, []chargedFact) {
	t.Helper()
	payload, err := Encode(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	result, err := PersistEvent(t.Context(), pool, event, payload)
	if err != nil {
		t.Fatalf("persist event: %v", err)
	}
	rows, err := pool.Query(t.Context(), `SELECT estimated_cost::text, charge::text,
	    discount_id::text, rate_card_revision_id::text
	    FROM olp.attempt_usage_facts WHERE request_id = $1::uuid ORDER BY attempt_ordinal`, event.RequestID)
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	defer rows.Close()
	var facts []chargedFact
	for rows.Next() {
		var fact chargedFact
		if err = rows.Scan(&fact.cost, &fact.charge, &fact.discount, &fact.cardRev); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		facts = append(facts, fact)
	}
	return result, facts
}

// TestIntegrationDiscountPrecedence prices one attempt under provider-, vendor-
// and kind-scoped multipliers at once; the most specific wins and list cost is
// untouched.
func TestIntegrationDiscountPrecedence(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "discounted", price: Price{ProviderKind: "openai", Model: "discounted",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	// Seed specific scopes first and the general one last so insertion order
	// cannot decide the answer.
	vendor := "openai"
	want := seedParityDiscount(t, pool, owner, "openai", nil, &provider, "0.1", observed.Add(-time.Hour))
	seedParityDiscount(t, pool, owner, "openai", &vendor, nil, "0.25", observed.Add(-time.Hour))
	seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.5", observed.Add(-time.Hour))
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "discounted", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	result, facts := persistCharge(t, pool, event)
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	// list cost 1000 x 3 / 1e6 = 0.003, provider discount 0.1 -> 0.0003.
	if facts[0].cost == nil || *facts[0].cost != "0.000300000000" {
		t.Fatalf("cost = %v, want 0.000300000000", facts[0].cost)
	}
	if facts[0].discount == nil || *facts[0].discount != want {
		t.Fatalf("discount = %v, want %v", facts[0].discount, want)
	}
	var discounted bool
	if err := pool.QueryRow(t.Context(), `SELECT 'discount' = ANY(pricing_components)
	    FROM olp.attempt_usage_facts WHERE request_id=$1::uuid`, event.RequestID).Scan(&discounted); err != nil {
		t.Fatalf("read components: %v", err)
	}
	if !discounted {
		t.Fatal("components lack the discount term")
	}
	// With no card, charge equals cost.
	if facts[0].charge == nil || *facts[0].charge != "0.000300000000" {
		t.Fatalf("charge = %v, want cost", facts[0].charge)
	}
}

// TestIntegrationDiscountEffectiveTime applies only discounts already in
// force, preferring the newest one in a scope.
func TestIntegrationDiscountEffectiveTime(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "discounted", price: Price{ProviderKind: "openai", Model: "discounted",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	want := seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.6", observed.Add(-time.Minute))
	seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.9", observed.Add(-2*time.Hour))
	// A scheduled multiplier must not apply yet.
	seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.01", observed.Add(time.Hour))
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "discounted", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	_, facts := persistCharge(t, pool, event)
	if facts[0].cost == nil || *facts[0].cost != "0.001800000000" {
		t.Fatalf("cost = %v, want 0.001800000000", facts[0].cost)
	}
	if facts[0].discount == nil || *facts[0].discount != want {
		t.Fatalf("discount = %v, want %v", facts[0].discount, want)
	}
}

// TestIntegrationDiscountPinning honours a pinned multiplier exactly — null
// included — while an unpinned attempt selects at its observed time.
func TestIntegrationDiscountPinning(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "a", price: Price{ProviderKind: "openai", Model: "a",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
		{model: "b", price: Price{ProviderKind: "openai", Model: "b",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
		{model: "c", price: Price{ProviderKind: "openai", Model: "c",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	active := seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.9", observed.Add(-time.Hour))
	scheduled := seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.1", observed.Add(time.Hour))
	usage := AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "a", usage: usage}, {model: "b", usage: usage}, {model: "c", usage: usage},
	})
	pin := func(attempt int, pinned bool, id *string) {
		raw, err := json.Marshal(map[string]any{"digest": "d", "strategy": "s",
			"discount_pinned": pinned, "discount_id": id})
		if err != nil {
			t.Fatal(err)
		}
		message := json.RawMessage(raw)
		event.Attempts[attempt].Routing = &Routing{Policy: &message}
	}
	// The scheduled discount honoured exactly despite not being in force.
	pin(0, true, &scheduled)
	// A pinned null discounts nothing even though one is in force.
	pin(1, true, nil)
	// No pin: the observed-time selection wins.
	pin(2, false, nil)
	_, facts := persistCharge(t, pool, event)
	if facts[0].cost == nil || *facts[0].cost != "0.000300000000" {
		t.Fatalf("pinned cost = %v, want 0.000300000000", facts[0].cost)
	}
	if facts[0].discount == nil || *facts[0].discount != scheduled {
		t.Fatalf("pinned discount = %v, want %v", facts[0].discount, scheduled)
	}
	if facts[1].cost == nil || *facts[1].cost != "0.003000000000" || facts[1].discount != nil {
		t.Fatalf("null pin: cost %v discount %v, want full price and no discount", facts[1].cost, facts[1].discount)
	}
	if facts[2].cost == nil || *facts[2].cost != "0.002700000000" {
		t.Fatalf("unpinned cost = %v, want 0.002700000000", facts[2].cost)
	}
	if facts[2].discount == nil || *facts[2].discount != active {
		t.Fatalf("unpinned discount = %v, want %v", facts[2].discount, active)
	}
}

// TestIntegrationCommittedDiscount keeps a committed attempt at zero marginal
// cost whatever multiplier is in force.
func TestIntegrationCommittedDiscount(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedCommittedProvider(t, pool, owner, "300", "day")
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "committed-model", price: Price{ProviderKind: "openai", Model: "committed-model",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.5", observed.Add(-time.Hour))
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "committed-model", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	_, facts := persistCharge(t, pool, event)
	if facts[0].cost == nil || *facts[0].cost != "0.000000000000" {
		t.Fatalf("committed cost = %v, want 0", facts[0].cost)
	}
}

// TestIntegrationChargePrecedence resolves the consumer's card project before
// organization before installation, and the newest in-force revision within it.
func TestIntegrationChargePrecedence(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	organization, project, key := seedParityProjectKey(t, pool, owner)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "priced", price: Price{ProviderKind: "openai", Model: "priced",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	seedParityCard(t, pool, owner, nil, nil, "2", observed.Add(-time.Hour))
	seedParityCard(t, pool, owner, &organization, nil, "3", observed.Add(-time.Hour))
	cardID, _ := seedParityCard(t, pool, owner, nil, &project, "4", observed.Add(-time.Hour))
	// A newer in-force revision replaces the card's own terms wholly.
	newRev := uuid.NewString()
	seedParityCardRevision(t, pool, owner, cardID, newRev, 2, "5", observed.Add(-time.Minute))
	// A scheduled revision must not apply.
	seedParityCardRevision(t, pool, owner, cardID, uuid.NewString(), 3, "9", observed.Add(time.Hour))
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "priced", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	result, facts := persistCharge(t, pool, event)
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	// cost 0.003 x project card's newest in-force markup 5.
	if facts[0].charge == nil || *facts[0].charge != "0.015000000000" {
		t.Fatalf("charge = %v, want 0.015000000000", facts[0].charge)
	}
	if facts[0].cardRev == nil || *facts[0].cardRev != newRev {
		t.Fatalf("card revision = %v, want %v", facts[0].cardRev, newRev)
	}
}

// TestIntegrationChargeExplicit prices a consumer from a card's own rate
// instead of cost x markup: discounts and commitments never apply to it, and
// it prices even what the provider could not.
func TestIntegrationChargeExplicit(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	_, project, key := seedParityProjectKey(t, pool, owner)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	cardID, revID := seedParityCard(t, pool, owner, nil, &project, "10", observed.Add(-time.Hour))
	_ = cardID
	seedParityCardPrice(t, pool, revID, Price{ProviderKind: "openai", Model: "mystery",
		Operation: "generation", InputPerMillion: rate("7"), Currency: "USD"})
	event := parityEvent(t, key, provider, observed, []parityCase{
		// No provider price covers this model: cost NULL, charge from the card.
		{model: "mystery", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	_, facts := persistCharge(t, pool, event)
	if facts[0].cost != nil {
		t.Fatalf("cost = %v, want NULL", facts[0].cost)
	}
	if facts[0].charge == nil || *facts[0].charge != "0.007000000000" {
		t.Fatalf("charge = %v, want 0.007000000000", facts[0].charge)
	}
	if facts[0].cardRev == nil || *facts[0].cardRev != revID {
		t.Fatalf("card revision = %v, want %v", facts[0].cardRev, revID)
	}
}

// TestIntegrationChargeNullCharge leaves the charge NULL when the consumer's
// card marks up a cost the provider never priced, and counts it uncharged.
func TestIntegrationChargeNullCharge(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	_, project, key := seedParityProjectKey(t, pool, owner)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	cardID, revID := seedParityCard(t, pool, owner, nil, &project, "2", observed.Add(-time.Hour))
	_ = cardID
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "mystery", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	_, facts := persistCharge(t, pool, event)
	if facts[0].charge != nil {
		t.Fatalf("charge = %v, want NULL", facts[0].charge)
	}
	if facts[0].cardRev == nil || *facts[0].cardRev != revID {
		t.Fatalf("card revision = %v, want %v", facts[0].cardRev, revID)
	}
	var uncharged bool
	if err := pool.QueryRow(t.Context(), `SELECT charge IS NULL AND charge_status <> 'not_billable'
	    FROM olp.attempt_usage_facts WHERE request_id=$1::uuid`, event.RequestID).Scan(&uncharged); err != nil {
		t.Fatal(err)
	}
	if !uncharged {
		t.Fatal("the attempt was not recorded as an uncharged billable one")
	}
}

// TestIntegrationChargeKeyless installs an installation card and charges the
// keyless attempt through it.
func TestIntegrationChargeKeyless(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "priced", price: Price{ProviderKind: "openai", Model: "priced",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	seedParityCard(t, pool, owner, nil, nil, "2", observed.Add(-time.Hour))
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "priced", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	_, facts := persistCharge(t, pool, event)
	if facts[0].charge == nil || *facts[0].charge != "0.006000000000" {
		t.Fatalf("charge = %v, want 0.006000000000", facts[0].charge)
	}
	if facts[0].cardRev == nil {
		t.Fatal("installation card did not apply")
	}
}

// TestIntegrationChargeNoCard leaves the charge identical to cost when the
// consumer resolves to no card.
func TestIntegrationChargeNoCard(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "priced", price: Price{ProviderKind: "openai", Model: "priced",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "priced", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	_, facts := persistCharge(t, pool, event)
	if facts[0].charge == nil || *facts[0].charge != "0.003000000000" {
		t.Fatalf("charge = %v, want 0.003000000000", facts[0].charge)
	}
	if facts[0].cardRev != nil {
		t.Fatalf("card revision = %v, want NULL", facts[0].cardRev)
	}
}

// TestIntegrationChargeRollup carries charges and uncharged counts through the
// hourly bucket.
func TestIntegrationChargeRollup(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	_, project, key := seedParityProjectKey(t, pool, owner)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "priced", price: Price{ProviderKind: "openai", Model: "priced",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	seedParityCard(t, pool, owner, nil, &project, "2", observed.Add(-time.Hour))
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "priced", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	payload, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = PersistEvent(t.Context(), pool, event, payload); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, facts, err := rollUpAttemptUsage(t.Context(), conn.Conn(), observed.Truncate(time.Hour).Add(time.Hour))
	conn.Release()
	if err != nil || facts != 1 {
		t.Fatalf("rollup folded %d facts: %v", facts, err)
	}
	var charge, uncharged *string
	if err = pool.QueryRow(t.Context(), `SELECT charge::text, uncharged_attempt_count::text
	    FROM olp.attempt_usage_hourly WHERE request_count > 0 LIMIT 1`).Scan(&charge, &uncharged); err != nil {
		t.Fatalf("read hourly: %v", err)
	}
	if charge == nil || *charge != "0.006000000000" || uncharged == nil || *uncharged != "0" {
		t.Fatalf("hourly charge %v uncharged %v, want 0.006 / 0", charge, uncharged)
	}
}

// TestIntegrationDiscountGoMirror confirms the Go mirror agrees with SQL on a
// discounted windowed, batched price.
func TestIntegrationDiscountGoMirror(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	at := time.Now().UTC()
	price := Price{ProviderKind: "openai", Model: "mirror", Operation: "generation",
		InputPerMillion: rate("3"), BatchMultiplier: rate("0.3"), Currency: "USD",
		Windows: &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
			{Days: []int{1, 2, 3, 4, 5, 6, 7}, Start: "00:00", End: "24:00", Multiplier: "1.5"}}}}
	seedParityPrices(t, pool, owner, at.Add(-time.Hour), []parityCase{{model: "mirror", price: price}})
	seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.5", at.Add(-time.Hour))
	usage := AttemptUsage{Observed: true, Complete: true, Batch: true, InputTokens: tokens(1000)}
	event := parityEvent(t, key, provider, at, []parityCase{{model: "mirror", usage: usage}})
	event.Attempts[0].StartedAt = at.Add(-time.Second)
	result, facts := persistCharge(t, pool, event)
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	// Mirror: same usage, window 1.5, batch 0.3, discount 0.5.
	discount := &RoutingDiscount{ID: "d", ProviderKind: "openai", Multiplier: "0.5"}
	got, ok := (&RoutingPrice{Price: price}).Cost(usage, at.Add(-time.Second), true, discount)
	if !ok {
		t.Fatal("mirror unpriced")
	}
	want := got.String() + "000000"[:12-len(strings.SplitN(got.String(), ".", 2)[1])]
	if facts[0].cost == nil || *facts[0].cost != want {
		t.Fatalf("mirror cost = %v, want %v", facts[0].cost, want)
	}
}

// TestIntegrationChargePinning honours a pinned card revision exactly,
// overriding what precedence would have selected for the consumer.
func TestIntegrationChargePinning(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	_, project, key := seedParityProjectKey(t, pool, owner)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{
		{model: "priced", price: Price{ProviderKind: "openai", Model: "priced",
			Operation: "generation", InputPerMillion: rate("3"), Currency: "USD"}},
	})
	_, installRev := seedParityCard(t, pool, owner, nil, nil, "2", observed.Add(-time.Hour))
	seedParityCard(t, pool, owner, nil, &project, "4", observed.Add(-time.Hour))
	event := parityEvent(t, key, provider, observed, []parityCase{
		{model: "priced", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(1000)}},
	})
	pin, err := json.Marshal(map[string]any{"digest": "d", "strategy": "s",
		"rate_card_pinned": true, "rate_card_revision_id": installRev})
	if err != nil {
		t.Fatal(err)
	}
	message := json.RawMessage(pin)
	event.Attempts[0].Routing = &Routing{Policy: &message}
	_, facts := persistCharge(t, pool, event)
	// 0.003 x pinned installation markup 2, not the project card's 4.
	if facts[0].charge == nil || *facts[0].charge != "0.006000000000" {
		t.Fatalf("pinned charge = %v, want 0.006000000000", facts[0].charge)
	}
	if facts[0].cardRev == nil || *facts[0].cardRev != installRev {
		t.Fatalf("card revision = %v, want %v", facts[0].cardRev, installRev)
	}
}

//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/usage"
)

// acctID returns a fresh identifier for a fixture row.
func acctID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new identifier: %v", err)
	}
	return id.String()
}

func acctPtr[T any](value T) *T { return &value }

// acctPool opens a migrated database of its own so one suite's rows cannot
// disturb another's aggregates.
func acctPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := accessDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func acctExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %.60s: %v", sql, err)
	}
}

// acctFixture is the catalogue every accounting test needs: an owner, a key to
// charge, and a provider with an active revision to price against.
type acctFixture struct {
	Pool     *pgxpool.Pool
	User     string
	Key      string
	Provider string
	Revision string
}

func acctSeed(t *testing.T, pool *pgxpool.Pool) acctFixture {
	t.Helper()
	fixture := acctFixture{Pool: pool, User: acctID(t), Key: acctID(t)}
	acctExec(t, pool, `INSERT INTO olp.users (id, email, display_name, role, etag)
        VALUES ($1::uuid, $2, 'Accounting Owner', 'owner', $3::uuid)`,
		fixture.User, "acct-"+fixture.User+"@example.test", acctID(t))
	acctExec(t, pool, `INSERT INTO olp.api_keys
            (id, lookup_id, digest, name, created_by, policy, etag)
        VALUES ($1::uuid, $2, $3, 'accounting', $4::uuid, '{}'::jsonb, $5::uuid)`,
		fixture.Key, "lookup-"+fixture.Key, []byte("digest"), fixture.User, acctID(t))
	fixture.Provider, fixture.Revision = acctProvider(t, fixture, "openai",
		`{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{"vendor_id":"openai"}}`)
	return fixture
}

// acctProvider adds a provider whose stored configuration is the one the
// pricing selection reads, and activates a revision carrying the same shape.
func acctProvider(t *testing.T, fixture acctFixture, kind, configuration string) (string, string) {
	t.Helper()
	provider, revision := acctID(t), acctID(t)
	acctExec(t, fixture.Pool, `INSERT INTO olp.providers
            (id, name, kind, state, configuration, etag, slots_etag, active_revision,
             active_revision_id, created_by)
        VALUES ($1::uuid, $2, $3, 'active', $4::jsonb, $5::uuid, $6::uuid, 1, $7::uuid, $8::uuid)`,
		provider, "provider-"+provider, kind, configuration, acctID(t), acctID(t), revision, fixture.User)
	acctExec(t, fixture.Pool, `INSERT INTO olp.provider_revisions
            (id, provider_id, revision, name, configuration, models, slots, source_etag, activated_by)
        VALUES ($1::uuid, $2::uuid, 1, $3, $4::jsonb, '[]'::jsonb, '[]'::jsonb, $5::uuid, $6::uuid)`,
		revision, provider, "provider-"+provider, configuration, acctID(t), fixture.User)
	return provider, revision
}

// acctPrice is one row of a pricing catalogue. A nil rate is a dimension the
// revision does not price, which is how an incomplete price is expressed.
type acctPrice struct {
	Kind       string
	VendorID   *string
	ProviderID *string
	Model      string
	Operation  string
	Input      *string
	Output     *string
	Cached     *string
	Unit       *string
	Write      *string
	Write5M    *string
	Write1H    *string
}

func acctPricing(t *testing.T, fixture acctFixture, revision int,
	effectiveAt time.Time, prices ...acctPrice) string {
	t.Helper()
	id := acctID(t)
	acctExec(t, fixture.Pool, `INSERT INTO olp.pricing_revisions (id, revision, effective_at, created_by)
        VALUES ($1::uuid, $2, $3, $4::uuid)`, id, revision, effectiveAt, fixture.User)
	for _, price := range prices {
		acctExec(t, fixture.Pool, `INSERT INTO olp.prices
                (pricing_revision_id, provider_kind, model, operation, input_per_million,
                 output_per_million, cached_input_per_million, cache_write_input_per_million,
                 cache_write_5m_input_per_million, cache_write_1h_input_per_million,
                 unit_price, currency, provider_id, vendor_id)
            VALUES ($1::uuid, $2, $3, $4, $5::text::numeric, $6::text::numeric, $7::text::numeric,
                    $8::text::numeric, $9::text::numeric, $10::text::numeric,
                    $11::text::numeric, 'USD', $12::uuid, $13)`,
			id, price.Kind, price.Model, price.Operation, price.Input, price.Output,
			price.Cached, price.Write, price.Write5M, price.Write1H,
			price.Unit, price.ProviderID, price.VendorID)
	}
	return id
}

// acctObserved is usage the provider reported in full.
func acctObserved(input, output int64, cached *int64, media *string) *usage.AttemptUsage {
	return &usage.AttemptUsage{
		Observed: true, Complete: true,
		InputTokens: acctPtr(input), OutputTokens: acctPtr(output),
		CachedInputTokens: cached, MediaUnits: media,
	}
}

// acctSettled is an attempt that produced no usage and never will.
func acctSettled() *usage.AttemptUsage { return &usage.AttemptUsage{Complete: true} }

// acctUncertain is an attempt whose charge can never be settled.
func acctUncertain() *usage.AttemptUsage { return &usage.AttemptUsage{BillingUncertain: true} }

func acctAttempt(t *testing.T, provider string, ordinal int, model string,
	status int, evidence *usage.AttemptUsage) usage.Attempt {
	t.Helper()
	started := time.Now().UTC().Add(-time.Second)
	return usage.Attempt{
		ID: acctID(t), Ordinal: ordinal, ProviderID: provider, UpstreamModel: model,
		StartedAt: started, CompletedAt: started.Add(500 * time.Millisecond),
		StatusCode: acctPtr(status), Committed: status >= 200 && status <= 299,
		LatencyMS: 500, Usage: evidence,
	}
}

type acctEventOptions struct {
	Operation  string
	Surface    string
	ObservedAt time.Time
	Attempts   []usage.Attempt
}

// acctEvent assembles an event whose request level target mirrors its final
// attempt, which is what the wire contract requires.
func acctEvent(t *testing.T, fixture acctFixture, options acctEventOptions) *usage.Event {
	t.Helper()
	if options.Operation == "" {
		options.Operation = "generation"
	}
	if options.Surface == "" {
		options.Surface = "openai"
	}
	if options.ObservedAt.IsZero() {
		options.ObservedAt = time.Now().UTC()
	}
	event := &usage.Event{
		Version: usage.WireVersion, EventID: acctID(t), RequestID: acctID(t),
		RuntimeGenerationID: acctID(t), APIKeyID: fixture.Key, RouteSlug: "chat",
		Operation: options.Operation, Surface: options.Surface,
		RequestStartedAt: options.ObservedAt.Add(-time.Second), RequestCompletedAt: options.ObservedAt,
		ObservedAt: options.ObservedAt, StatusCode: acctPtr(200), LatencyMS: 1000,
		Attempts: options.Attempts,
	}
	if len(options.Attempts) > 0 {
		final := options.Attempts[len(options.Attempts)-1]
		event.ProviderID = acctPtr(final.ProviderID)
		event.UpstreamModel = acctPtr(final.UpstreamModel)
		event.Committed = final.Committed
		event.StatusCode = final.StatusCode
	}
	return event
}

// acctPersist persists an event exactly as the consumer would, from the bytes
// that would have travelled the stream.
func acctPersist(t *testing.T, fixture acctFixture, event *usage.Event) usage.Persisted {
	t.Helper()
	result, err := acctPersistErr(t, fixture, event)
	if err != nil {
		t.Fatalf("persist event: %v", err)
	}
	return result
}

func acctPersistErr(t *testing.T, fixture acctFixture, event *usage.Event) (usage.Persisted, error) {
	t.Helper()
	payload, err := usage.Encode(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return usage.PersistEvent(t.Context(), fixture.Pool, event, payload)
}

// acctFact is one attempt's accounting row.
type acctFact struct {
	ChargeStatus    string
	Cost            *string
	Unpriced        bool
	UsageComplete   bool
	Currency        *string
	PricingRevision *string
	Request         bool
	Provider        bool
	Model           bool
	Target          bool
	RequestUnpriced bool
	RequestPartial  bool
}

func acctLoadFact(t *testing.T, fixture acctFixture, requestID string, ordinal int) acctFact {
	t.Helper()
	var fact acctFact
	err := fixture.Pool.QueryRow(t.Context(), `SELECT charge_status, estimated_cost::text, unpriced,
            usage_complete, currency, pricing_revision_id::text, request_counted,
            provider_request_counted, model_request_counted, target_request_counted,
            request_unpriced_counted, request_incomplete_counted
        FROM olp.attempt_usage_facts WHERE request_id = $1::uuid AND attempt_ordinal = $2`,
		requestID, ordinal).Scan(&fact.ChargeStatus, &fact.Cost, &fact.Unpriced,
		&fact.UsageComplete, &fact.Currency, &fact.PricingRevision, &fact.Request,
		&fact.Provider, &fact.Model, &fact.Target, &fact.RequestUnpriced, &fact.RequestPartial)
	if err != nil {
		t.Fatalf("load fact %s/%d: %v", requestID, ordinal, err)
	}
	return fact
}

// acctSameMoney compares two decimal strings as the database compares them, so
// a difference in scale is not read as a difference in money.
func acctSameMoney(t *testing.T, fixture acctFixture, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("money = nil, want %s", want)
	}
	var equal bool
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT $1::text::numeric = $2::text::numeric`,
		*got, want).Scan(&equal); err != nil {
		t.Fatalf("compare money: %v", err)
	}
	if !equal {
		t.Fatalf("money = %s, want %s", *got, want)
	}
}

func acctCount(t *testing.T, fixture acctFixture, sql string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := fixture.Pool.QueryRow(t.Context(), sql, args...).Scan(&count); err != nil {
		t.Fatalf("count %.60s: %v", sql, err)
	}
	return count
}

// acctWindow reads the durable budget window the persistence updated.
func acctWindow(t *testing.T, fixture acctFixture, kind string) (string, int64) {
	t.Helper()
	var accrued string
	var unpriced int64
	err := fixture.Pool.QueryRow(t.Context(), `SELECT accrued::text, unpriced_attempts
        FROM olp.api_key_cost_windows WHERE api_key_id = $1::uuid AND window_kind = $2`,
		fixture.Key, kind).Scan(&accrued, &unpriced)
	if err != nil {
		t.Fatalf("load %s window: %v", kind, err)
	}
	return accrued, unpriced
}

func TestAccountingPersistsPricedAttemptsAndIsReplaySafe(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	observed := time.Now().UTC().Add(-time.Minute)
	revision := acctPricing(t, fixture, 1, observed.Add(-time.Hour),
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-4o",
			Operation: "generation", Input: acctPtr("3"), Output: acctPtr("15"),
			Cached: acctPtr("0.75")},
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-4o-mini",
			Operation: "generation", Input: acctPtr("300"), Output: acctPtr("300")},
		acctPrice{Kind: "openai", ProviderID: acctPtr(fixture.Provider), Model: "gpt-4o-mini",
			Operation: "generation", Input: acctPtr("1"), Output: acctPtr("2")})

	event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed, Attempts: []usage.Attempt{
		acctAttempt(t, fixture.Provider, 1, "gpt-4o-mini", 200, acctObserved(1000, 500, nil, nil)),
		acctAttempt(t, fixture.Provider, 2, "gpt-4o", 200,
			acctObserved(1000, 500, acctPtr(int64(400)), nil)),
	}})
	result := acctPersist(t, fixture, event)
	if result.Outcome != usage.PersistOutcomePersisted {
		t.Fatalf("outcome = %v, want persisted", result.Outcome)
	}
	if len(result.CostSnapshots) == 0 {
		t.Fatal("persisted attempts reported no spend")
	}
	acctSameMoney(t, fixture, &result.CostSnapshots[0].DailyAccrued, "0.0116")
	acctSameMoney(t, fixture, &result.CostSnapshots[0].MonthlyAccrued, "0.0116")
	if result.CostSnapshots[0].UnpricedAttempts != 0 {
		t.Fatalf("unpriced attempts = %d, want 0", result.CostSnapshots[0].UnpricedAttempts)
	}

	// The provider scoped price outbids the vendor wide one for the same model.
	first := acctLoadFact(t, fixture, event.RequestID, 1)
	acctSameMoney(t, fixture, first.Cost, "0.002")
	if first.ChargeStatus != "billable" || first.Unpriced || !first.UsageComplete {
		t.Fatalf("first attempt = %+v, want a settled billable charge", first)
	}
	if first.PricingRevision == nil || *first.PricingRevision != revision {
		t.Fatalf("first attempt priced against %v, want %s", first.PricingRevision, revision)
	}
	if first.Currency == nil || *first.Currency != "USD" {
		t.Fatalf("first attempt currency = %v, want USD", first.Currency)
	}
	// Cached input bills the discounted tier and never double counts.
	second := acctLoadFact(t, fixture, event.RequestID, 2)
	acctSameMoney(t, fixture, second.Cost, "0.0096")

	// Exactly one attempt carries each dimension's request count.
	if !first.Request || !first.Provider || !first.Model || !first.Target {
		t.Fatalf("first attempt markers = %+v, want every dimension counted", first)
	}
	if second.Request || second.Provider {
		t.Fatalf("second attempt recounted its request or provider: %+v", second)
	}
	if !second.Model || !second.Target {
		t.Fatalf("second attempt did not count its own model: %+v", second)
	}
	if first.RequestUnpriced || first.RequestPartial {
		t.Fatalf("a fully priced request was marked incomplete: %+v", first)
	}

	if count := acctCount(t, fixture, `SELECT attempt_count FROM olp.requests WHERE id = $1::uuid`,
		event.RequestID); count != 2 {
		t.Fatalf("request attempt count = %d, want 2", count)
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp.attempts WHERE request_id = $1::uuid`,
		event.RequestID); count != 2 {
		t.Fatalf("attempts = %d, want 2", count)
	}
	var status string
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT status
        FROM olp.request_metadata_event_receipts WHERE event_id = $1::uuid`,
		event.EventID).Scan(&status); err != nil {
		t.Fatalf("load receipt: %v", err)
	}
	if status != "fact_persisted" {
		t.Fatalf("receipt status = %s, want fact_persisted", status)
	}
	daily, unpriced := acctWindow(t, fixture, "day")
	acctSameMoney(t, fixture, &daily, "0.0116")
	if unpriced != 0 {
		t.Fatalf("daily unpriced attempts = %d, want 0", unpriced)
	}

	// A redelivery of the same bytes must not charge twice.
	replay := acctPersist(t, fixture, event)
	if replay.Outcome != usage.PersistOutcomeDuplicate {
		t.Fatalf("replay outcome = %v, want duplicate", replay.Outcome)
	}
	if len(replay.CostSnapshots) != 0 {
		t.Fatal("a duplicate reported spend")
	}
	daily, _ = acctWindow(t, fixture, "day")
	acctSameMoney(t, fixture, &daily, "0.0116")
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp.attempt_usage_facts
        WHERE request_id = $1::uuid`, event.RequestID); count != 2 {
		t.Fatalf("facts after replay = %d, want 2", count)
	}

	// A second event claiming the same request is a contradiction, not a replay.
	conflicting := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
		Attempts: []usage.Attempt{
			acctAttempt(t, fixture.Provider, 1, "gpt-4o", 200, acctObserved(1, 1, nil, nil)),
		}})
	conflicting.RequestID = event.RequestID
	if _, err := acctPersistErr(t, fixture, conflicting); !errors.Is(err, usage.ErrInvalidEvent) {
		t.Fatalf("conflicting event error = %v, want ErrInvalidEvent", err)
	}
}

func TestAccountingRejectsEventsOutsideTheReplayWindow(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	acctPricing(t, fixture, 1, time.Now().UTC().Add(-30*24*time.Hour),
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-4o",
			Operation: "generation", Input: acctPtr("3"), Output: acctPtr("15")})
	cases := []struct {
		name       string
		observedAt time.Time
	}{
		{name: "older than the replay horizon",
			observedAt: time.Now().UTC().Add(-8 * 24 * time.Hour)},
		{name: "further ahead than the accepted clock skew",
			observedAt: time.Now().UTC().Add(10 * time.Minute)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			event := acctEvent(t, fixture, acctEventOptions{ObservedAt: test.observedAt,
				Attempts: []usage.Attempt{
					acctAttempt(t, fixture.Provider, 1, "gpt-4o", 200, acctObserved(10, 10, nil, nil)),
				}})
			result := acctPersist(t, fixture, event)
			if result.Outcome != usage.PersistOutcomeRejectedOutsideReplayWindow {
				t.Fatalf("outcome = %v, want rejected", result.Outcome)
			}
			if count := acctCount(t, fixture, `SELECT count(*) FROM olp.attempt_usage_facts
                WHERE request_id = $1::uuid`, event.RequestID); count != 0 {
				t.Fatalf("facts = %d, want none", count)
			}
			if count := acctCount(t, fixture, `SELECT count(*) FROM olp.requests
                WHERE id = $1::uuid`, event.RequestID); count != 0 {
				t.Fatalf("requests = %d, want none", count)
			}
			var status string
			if err := fixture.Pool.QueryRow(t.Context(), `SELECT status
                FROM olp.request_metadata_event_receipts WHERE event_id = $1::uuid`,
				event.EventID).Scan(&status); err != nil {
				t.Fatalf("load receipt: %v", err)
			}
			if status != "rejected" {
				t.Fatalf("receipt status = %s, want rejected", status)
			}
			// The rejection is admitted as a gap of unknown size rather than
			// being silently dropped.
			if count := acctCount(t, fixture, `SELECT count(*) FROM olp.request_metadata_ingestion_gaps
                WHERE reason = 'request_metadata_event_outside_replay_window'
                  AND certainty = 'lower_bound' AND event_count = 0`); count == 0 {
				t.Fatal("the rejection recorded no completeness gap")
			}
			// Redelivering the rejected event is recognised, not re-rejected.
			replay := acctPersist(t, fixture, event)
			if replay.Outcome != usage.PersistOutcomeDuplicate {
				t.Fatalf("replay outcome = %v, want duplicate", replay.Outcome)
			}
		})
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp.api_key_cost_windows
        WHERE api_key_id = $1::uuid`, fixture.Key); count != 0 {
		t.Fatalf("rejected events opened %d budget windows, want none", count)
	}
}

// acctMedia is usage measured in media units rather than tokens.
func acctMedia(units string) *usage.AttemptUsage {
	return &usage.AttemptUsage{Observed: true, Complete: true, MediaUnits: acctPtr(units)}
}

func TestAccountingPricingProvenance(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	gemini, geminiRevision := acctProvider(t, fixture, "gemini",
		`{"kind":"gemini","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{}}`)
	observed := time.Now().UTC().Add(-time.Minute)
	older := acctPricing(t, fixture, 1, observed.Add(-2*time.Hour),
		// The connector kind has no vendor recorded, so the catalogue's default
		// name for that kind has to match.
		acctPrice{Kind: "gemini", VendorID: acctPtr("google"), Model: "gemini-2.5-pro",
			Operation: "generation", Input: acctPtr("1"), Output: acctPtr("2")},
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-4o",
			Operation: "generation", Input: acctPtr("10"), Output: acctPtr("10")})
	newer := acctPricing(t, fixture, 2, observed.Add(-time.Hour),
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-4o",
			Operation: "generation", Input: acctPtr("100"), Output: acctPtr("100")},
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-image-1",
			Operation: "image_generation", Unit: acctPtr("0.000001")},
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "half-priced",
			Operation: "generation", Input: acctPtr("5")})

	t.Run("a connector without a recorded vendor prices against its kind's default", func(t *testing.T) {
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
			Attempts: []usage.Attempt{
				acctAttempt(t, gemini, 1, "gemini-2.5-pro", 200, acctObserved(1000, 1000, nil, nil)),
			}})
		event.Attempts[0].Routing = &usage.Routing{ProviderRevisionID: geminiRevision}
		event.Surface = "gemini"
		acctPersist(t, fixture, event)
		fact := acctLoadFact(t, fixture, event.RequestID, 1)
		acctSameMoney(t, fixture, fact.Cost, "0.003")
		if fact.PricingRevision == nil || *fact.PricingRevision != older {
			t.Fatalf("priced against %v, want the only revision carrying that model", fact.PricingRevision)
		}
	})

	t.Run("the newest effective revision prices an unpinned attempt", func(t *testing.T) {
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
			Attempts: []usage.Attempt{
				acctAttempt(t, fixture.Provider, 1, "gpt-4o", 200, acctObserved(1000, 1000, nil, nil)),
			}})
		acctPersist(t, fixture, event)
		fact := acctLoadFact(t, fixture, event.RequestID, 1)
		acctSameMoney(t, fixture, fact.Cost, "0.2")
		if fact.PricingRevision == nil || *fact.PricingRevision != newer {
			t.Fatalf("priced against %v, want %s", fact.PricingRevision, newer)
		}
	})

	t.Run("a pinned attempt keeps the catalogue the gateway quoted", func(t *testing.T) {
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
			Attempts: []usage.Attempt{
				acctAttempt(t, fixture.Provider, 1, "gpt-4o", 200, acctObserved(1000, 1000, nil, nil)),
			}})
		policy := json.RawMessage(`{"pricing_pinned":true,"vendor_id":"openai"}`)
		event.Attempts[0].Routing = &usage.Routing{ProviderRevisionID: fixture.Revision,
			PricingRevisionID: acctPtr(older), Policy: &policy}
		acctPersist(t, fixture, event)
		fact := acctLoadFact(t, fixture, event.RequestID, 1)
		acctSameMoney(t, fixture, fact.Cost, "0.02")
		if fact.PricingRevision == nil || *fact.PricingRevision != older {
			t.Fatalf("priced against %v, want the pinned %s", fact.PricingRevision, older)
		}
	})

	t.Run("a pin without the quoted vendor identity charges nothing", func(t *testing.T) {
		// A pin is a promise to bill exactly what was quoted. Without the
		// vendor the quote was scoped to, only a vendor neutral price can be
		// honoured, and charging a vendor price anyway would invent a rate.
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
			Attempts: []usage.Attempt{
				acctAttempt(t, fixture.Provider, 1, "gpt-4o", 200, acctObserved(1000, 1000, nil, nil)),
			}})
		event.Attempts[0].Routing = &usage.Routing{
			ProviderRevisionID: fixture.Revision, PricingRevisionID: acctPtr(older)}
		acctPersist(t, fixture, event)
		fact := acctLoadFact(t, fixture, event.RequestID, 1)
		if fact.Cost != nil || !fact.Unpriced || fact.PricingRevision != nil {
			t.Fatalf("fact = %+v, want an unpriced attempt with no catalogue", fact)
		}
	})

	t.Run("media units bill at their unit price without rounding", func(t *testing.T) {
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
			Operation: "image_generation", Attempts: []usage.Attempt{
				acctAttempt(t, fixture.Provider, 1, "gpt-image-1", 200, acctMedia("3")),
			}})
		acctPersist(t, fixture, event)
		acctSameMoney(t, fixture, acctLoadFact(t, fixture, event.RequestID, 1).Cost, "0.000003")
	})

	t.Run("a price missing a dimension the attempt used charges nothing", func(t *testing.T) {
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
			Attempts: []usage.Attempt{
				acctAttempt(t, fixture.Provider, 1, "half-priced", 200, acctObserved(1000, 1000, nil, nil)),
			}})
		result := acctPersist(t, fixture, event)
		fact := acctLoadFact(t, fixture, event.RequestID, 1)
		if fact.Cost != nil || !fact.Unpriced || fact.ChargeStatus != "billable" {
			t.Fatalf("fact = %+v, want a billable attempt recorded as unpriced", fact)
		}
		if !fact.RequestUnpriced {
			t.Fatal("the request was not marked as carrying an unpriced attempt")
		}
		// The snapshot carries the window totals, which now hold this attempt
		// and the pin that could not be honoured.
		if len(result.CostSnapshots) == 0 || result.CostSnapshots[0].UnpricedAttempts != 2 {
			t.Fatalf("snapshot = %+v, want two unpriced attempts so far", result.CostSnapshots[0])
		}
	})

	t.Run("attempts that cannot be settled are never charged", func(t *testing.T) {
		event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
			Attempts: []usage.Attempt{
				acctAttempt(t, fixture.Provider, 1, "gpt-4o", 503, acctSettled()),
				acctAttempt(t, fixture.Provider, 2, "gpt-4o", 200, acctUncertain()),
			}})
		event.Attempts[0].ErrorClass = acctPtr("upstream_unavailable")
		result := acctPersist(t, fixture, event)
		failed := acctLoadFact(t, fixture, event.RequestID, 1)
		if failed.ChargeStatus != "not_billable" || failed.Cost != nil || failed.Unpriced {
			t.Fatalf("failed attempt = %+v, want a settled non billable attempt", failed)
		}
		if !failed.UsageComplete {
			t.Fatal("a settled attempt was recorded as incomplete")
		}
		uncertain := acctLoadFact(t, fixture, event.RequestID, 2)
		if uncertain.ChargeStatus != "billing_uncertain" || uncertain.UsageComplete {
			t.Fatalf("uncertain attempt = %+v, want an unsettled charge", uncertain)
		}
		if uncertain.Cost != nil || !uncertain.Unpriced {
			t.Fatalf("uncertain attempt = %+v, want no charge and an unpriced mark", uncertain)
		}
		if !failed.RequestPartial {
			t.Fatal("the request was not marked as incomplete")
		}
		if len(result.CostSnapshots) == 0 || result.CostSnapshots[0].UnpricedAttempts != 3 {
			t.Fatalf("snapshot = %+v, want three unpriced attempts so far", result.CostSnapshots[0])
		}
	})

	daily, unpriced := acctWindow(t, fixture, "day")
	acctSameMoney(t, fixture, &daily, "0.223003")
	if unpriced != 0 {
		t.Fatalf("daily unpriced attempts = %d, want 0", unpriced)
	}
	if _, monthly := acctWindow(t, fixture, "month"); monthly != 3 {
		t.Fatalf("monthly unpriced attempts = %d, want 2", monthly)
	}
}

func TestAccountingRejectsCachedTokensWithoutTheirTotal(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	observed := time.Now().UTC().Add(-time.Minute)
	acctPricing(t, fixture, 1, observed.Add(-time.Hour),
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-4o",
			Operation: "generation", Input: acctPtr("3"), Output: acctPtr("15"),
			Cached: acctPtr("0.75")})

	event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
		Attempts: []usage.Attempt{
			acctAttempt(t, fixture.Provider, 1, "gpt-4o", 200, &usage.AttemptUsage{
				Observed: true, Complete: true,
				OutputTokens: acctPtr(int64(500)), CachedInputTokens: acctPtr(int64(400)),
			}),
		}})
	if _, err := acctPersistErr(t, fixture, event); !errors.Is(err, usage.ErrInvalidEvent) {
		t.Fatalf("persist err = %v, want ErrInvalidEvent", err)
	}
}

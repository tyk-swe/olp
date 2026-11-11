package pricebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// pricingLockID serialises pricing revision creation across replicas ("OLP_PR").
// Revision numbers are dense and consecutive, so two concurrent creators must
// not read the same maximum.
const pricingLockID = 0x4f4c_505f_5052

// MaxRevisionPrices bounds one revision. A catalogue larger than this is a
// mistake, and accepting it would make the transaction hold the pricing lock
// for an unbounded time.
const MaxRevisionPrices = 10000

// Revision is an immutable price list that takes effect at a point in time.
// Facts are priced against the revision in force when they were observed, so a
// later revision never restates history.
type Revision struct {
	ID          string    `json:"id"`
	Revision    int       `json:"revision"`
	EffectiveAt time.Time `json:"effective_at"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`

	SourceSnapshotID *string       `json:"source_snapshot_id"`
	SourceName       *string       `json:"source_name"`
	Prices           []rates.Price `json:"prices"`
}

// CreateRevision validates and stores one pricing revision inside the caller's
// mutation transaction. The caller owns the idempotency claim and the audit
// event; this owns the numbering, the installation currency, and every rule
// that makes a stored rate meaningful.
func CreateRevision(ctx context.Context, tx pgx.Tx, actor string, effectiveAt time.Time,
	prices []rates.Price, normalizer Normalizer) (Revision, error) {
	normalized, currency, err := validatePrices(prices, normalizer)
	if err != nil {
		return Revision{}, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(pricingLockID)); err != nil {
		return Revision{}, fmt.Errorf("lock pricing revisions: %w", err)
	}
	if err = reserveCurrency(ctx, tx, currency); err != nil {
		return Revision{}, err
	}
	var number int
	if err = tx.QueryRow(ctx,
		"SELECT COALESCE(MAX(revision), 0) + 1 FROM olp.pricing_revisions").Scan(&number); err != nil {
		return Revision{}, fmt.Errorf("number pricing revision: %w", err)
	}
	revision := Revision{
		ID:          access.NewID(),
		Revision:    number,
		EffectiveAt: effectiveAt.UTC(),
		CreatedBy:   actor,
		CreatedAt:   time.Now().UTC(),
		Prices:      normalized,
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.pricing_revisions
            (id, revision, effective_at, created_by, created_at) VALUES ($1, $2, $3, $4, $5)`,
		revision.ID, revision.Revision, revision.EffectiveAt, actor, revision.CreatedAt); err != nil {
		return Revision{}, fmt.Errorf("store pricing revision: %w", err)
	}
	for _, price := range normalized {
		if err = insertPrice(ctx, tx, revision.ID, price); err != nil {
			return Revision{}, err
		}
	}
	return revision, nil
}

// reserveCurrency enforces one currency per installation. The first revision
// sets it; a later revision in another currency is refused, because mixing them
// would make every total a lie no report could detect.
func reserveCurrency(ctx context.Context, tx pgx.Tx, currency string) error {
	var configured string
	err := tx.QueryRow(ctx,
		"SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton").Scan(&configured)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx,
			"INSERT INTO olp.pricing_currency (singleton, currency) VALUES (true, $1)",
			currency); err != nil {
			return fmt.Errorf("store installation pricing currency: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read installation pricing currency: %w", err)
	}
	if configured != currency {
		return invalid("prices",
			"Pricing must use the installation currency "+configured+".")
	}
	return nil
}

// insertPrice stores one rate. A provider-scoped override must reference a
// provider of the declared kind, or the override would never be selected and
// the operator would be left believing a rate is in force.
func insertPrice(ctx context.Context, tx pgx.Tx, revisionID string, price rates.Price) error {
	if price.ProviderID != nil {
		var kind string
		err := tx.QueryRow(ctx, "SELECT kind FROM olp.providers WHERE id=$1", *price.ProviderID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != price.ProviderKind) {
			return invalid("prices",
				"A pricing override must reference a provider of the declared kind.")
		}
		if err != nil {
			return fmt.Errorf("read pricing override provider: %w", err)
		}
	}
	if price.Windows != nil {
		// The zone was accepted by Go's LoadLocation; confirming it here keeps
		// a zone the database cannot resolve from ever reaching the accounting
		// function, where it would fail on every attempt priced by this row.
		var local time.Time
		if err := tx.QueryRow(ctx, "SELECT now() AT TIME ZONE $1", price.Windows.TimeZone).Scan(&local); err != nil {
			return invalid("prices", "The pricing window time zone is unknown.")
		}
	}
	toolCalls, err := priceJSON(price.ToolCalls)
	if err != nil {
		return err
	}
	tiers, err := priceJSON(price.Tiers)
	if err != nil {
		return err
	}
	windows, err := priceJSON(price.Windows)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.prices
            (pricing_revision_id, provider_kind, provider_id, model, operation,
             input_per_million, cached_input_per_million, output_per_million,
             cache_write_input_per_million, cache_write_5m_input_per_million,
             cache_write_1h_input_per_million,
             audio_input_per_million, cached_audio_input_per_million, audio_output_per_million,
             image_input_per_million, cached_image_input_per_million, image_output_per_million,
             per_request, batch_multiplier, tool_calls, tiers, windows,
             unit_price, currency, vendor_id, training_per_million, storage_per_gib_day, cache_storage_per_million_token_hour, container_per_session)
        VALUES ($1, $2, $3, $4, $5, $6::text::numeric, $7::text::numeric, $8::text::numeric,
                $9::text::numeric, $10::text::numeric, $11::text::numeric, $12::text::numeric,
                $13::text::numeric, $14::text::numeric, $15::text::numeric, $16::text::numeric,
                $17::text::numeric, $18::text::numeric, $19::text::numeric,
                $20::jsonb, $21::jsonb, $22::jsonb,
                $23, $24, $25, $26::text::numeric, $27::text::numeric, $28::text::numeric, $29::jsonb)`,
		revisionID, price.ProviderKind, price.ProviderID, price.Model, price.Operation,
		price.InputPerMillion, price.CachedInputPerMillion, price.OutputPerMillion,
		price.CacheWriteInputPerMillion, price.CacheWrite5MInputPerMillion,
		price.CacheWrite1HInputPerMillion,
		price.AudioInputPerMillion, price.CachedAudioInputPerMillion, price.AudioOutputPerMillion,
		price.ImageInputPerMillion, price.CachedImageInputPerMillion, price.ImageOutputPerMillion,
		price.PerRequest, price.BatchMultiplier,
		toolCalls, tiers, windows,
		price.UnitPrice, price.Currency, price.VendorID, price.TrainingPerMillion, price.StoragePerGiBDay, price.CacheStoragePerMillionTokenHour, OptionalJSON(price.ContainerPerSession))
	if err != nil {
		return fmt.Errorf("store price: %w", err)
	}
	return nil
}

// priceJSON renders a structured price component for its jsonb column, or nil
// for the NULL an absent component stores.
func priceJSON(value any) (any, error) {
	switch v := value.(type) {
	case map[string]string:
		if v == nil {
			return nil, nil
		}
	case []rates.PriceTier:
		if v == nil {
			return nil, nil
		}
	case *rates.PriceWindows:
		if v == nil {
			return nil, nil
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("store price component: %w", err)
	}
	return string(data), nil
}

const revisionColumns = `SELECT r.id::text, r.revision, r.effective_at, r.created_by::text,
        r.created_at, r.source_snapshot_id::text, src.name, p.vendor_id, p.provider_kind, p.provider_id::text, p.model, p.operation,
        p.input_per_million::text, p.cached_input_per_million::text, p.output_per_million::text,
        p.cache_write_input_per_million::text, p.cache_write_5m_input_per_million::text,
        p.cache_write_1h_input_per_million::text,
        p.audio_input_per_million::text, p.cached_audio_input_per_million::text,
        p.audio_output_per_million::text,
        p.image_input_per_million::text, p.cached_image_input_per_million::text,
        p.image_output_per_million::text,
        p.per_request::text, p.batch_multiplier::text,
        p.tool_calls::text, p.tiers::text, p.windows::text,
        p.unit_price::text, btrim(p.currency), p.training_per_million::text, p.storage_per_gib_day::text, p.cache_storage_per_million_token_hour::text, p.container_per_session::text
    FROM olp.pricing_revisions r
    LEFT JOIN olp.pricing_source_snapshots snap ON snap.id = r.source_snapshot_id
    LEFT JOIN olp.pricing_sources src ON src.id = snap.source_id
    LEFT JOIN olp.prices p ON p.pricing_revision_id = r.id
    WHERE r.id IN (SELECT id FROM olp.pricing_revisions
                    WHERE ($1::int IS NULL OR revision < $1) ORDER BY revision DESC LIMIT $2)
    ORDER BY r.revision DESC, p.provider_kind, p.provider_id NULLS FIRST, p.model, p.operation,
        p.vendor_id NULLS FIRST`

// ListRevisions pages revisions newest first, each with its full price list.
// The cursor is the revision number, which is dense and immutable.
func ListRevisions(ctx context.Context, q access.Queryer, beforeRevision *int, limit int) ([]Revision, *string, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := q.Query(ctx, revisionColumns, beforeRevision, int64(limit)+1)
	if err != nil {
		return nil, nil, fmt.Errorf("list pricing revisions: %w", err)
	}
	defer rows.Close()
	items := []Revision{}
	for rows.Next() {
		var revision Revision
		var price rates.Price
		var kind, model, operation, currency *string
		var toolCalls, tiers, windows, containerSessions *string
		if err = rows.Scan(&revision.ID, &revision.Revision, &revision.EffectiveAt,
			&revision.CreatedBy, &revision.CreatedAt, &revision.SourceSnapshotID, &revision.SourceName,
			&price.VendorID, &kind, &price.ProviderID,
			&model, &operation, &price.InputPerMillion, &price.CachedInputPerMillion,
			&price.OutputPerMillion, &price.CacheWriteInputPerMillion,
			&price.CacheWrite5MInputPerMillion, &price.CacheWrite1HInputPerMillion,
			&price.AudioInputPerMillion, &price.CachedAudioInputPerMillion,
			&price.AudioOutputPerMillion, &price.ImageInputPerMillion,
			&price.CachedImageInputPerMillion, &price.ImageOutputPerMillion,
			&price.PerRequest, &price.BatchMultiplier,
			&toolCalls, &tiers, &windows,
			&price.UnitPrice, &currency, &price.TrainingPerMillion, &price.StoragePerGiBDay, &price.CacheStoragePerMillionTokenHour, &containerSessions); err != nil {
			return nil, nil, fmt.Errorf("list pricing revisions: %w", err)
		}
		if err = rates.DecodeContainerRates(&price, containerSessions); err != nil {
			return nil, nil, err
		}
		if err = rates.UnmarshalPriceComponents(&price, toolCalls, tiers, windows, "pricing"); err != nil {
			return nil, nil, err
		}
		if len(items) == 0 || items[len(items)-1].ID != revision.ID {
			revision.EffectiveAt = revision.EffectiveAt.UTC()
			revision.CreatedAt = revision.CreatedAt.UTC()
			revision.Prices = []rates.Price{}
			items = append(items, revision)
		}
		if kind == nil {
			// A revision with no prices cannot be selected against; it is still
			// listed so the history stays contiguous.
			continue
		}
		if model == nil || operation == nil || currency == nil {
			return nil, nil, errors.New("stored price is invalid")
		}
		price.ProviderKind, price.Model, price.Operation, price.Currency = *kind, *model, *operation, *currency
		current := &items[len(items)-1]
		current.Prices = append(current.Prices, price)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list pricing revisions: %w", err)
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		token := strconv.Itoa(items[len(items)-1].Revision)
		next = &token
	}
	return items, next, nil
}

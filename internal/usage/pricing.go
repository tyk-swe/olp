package usage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

// pricingLockID serialises pricing revision creation across replicas ("OLP_PR").
// Revision numbers are dense and consecutive, so two concurrent creators must
// not read the same maximum.
const pricingLockID = 0x4f4c_505f_5052

// MaxRevisionPrices bounds one revision. A catalogue larger than this is a
// mistake, and accepting it would make the transaction hold the pricing lock
// for an unbounded time.
const MaxRevisionPrices = 10000

// providerKinds are the connector kinds a price may be scoped to. They match
// the database CHECK, so an unknown kind is refused with a field error rather
// than a constraint violation.
var providerKinds = []string{"openai", "anthropic", "gemini", "vertex_ai", "bedrock",
	"azure_openai", "openai_compatible"}

// priceOperations are the operations a price may be scoped to, and the
// operations a usage filter may name.
var priceOperations = []string{"generation", "embeddings", "token_count", "image_generation",
	"image_edit", "image_variation", "speech", "transcription", "video_create", "video_list",
	"video_get", "video_content", "video_delete", "moderation", "model_list", "model_get"}

func validOperation(value string) bool {
	return slices.Contains(priceOperations, value)
}

func validProviderKind(value string) bool {
	return slices.Contains(providerKinds, value)
}

// Price is one rate in a pricing revision. Rates are exact decimal strings:
// they are stored as `numeric` and multiplied by token counts in PostgreSQL, so
// no rounding ever happens in Go.
type Price struct {
	VendorID        *string `json:"vendor_id"`
	ProviderKind    string  `json:"provider_kind"`
	ProviderID      *string `json:"provider_id"`
	Model           string  `json:"model"`
	Operation       string  `json:"operation"`
	InputPerMillion *string `json:"input_per_million"`
	// CachedInputPerMillion rates the cached share of the input tokens. Absent
	// means cached tokens bill at the full input rate.
	CachedInputPerMillion *string `json:"cached_input_per_million"`
	OutputPerMillion      *string `json:"output_per_million"`
	UnitPrice             *string `json:"unit_price"`
	Currency              string  `json:"currency"`
}

// Revision is an immutable price list that takes effect at a point in time.
// Facts are priced against the revision in force when they were observed, so a
// later revision never restates history.
type Revision struct {
	ID          string    `json:"id"`
	Revision    int       `json:"revision"`
	EffectiveAt time.Time `json:"effective_at"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	Prices      []Price   `json:"prices"`
}

// CreateRevision validates and stores one pricing revision inside the caller's
// mutation transaction. The caller owns the idempotency claim and the audit
// event; this owns the numbering, the installation currency, and every rule
// that makes a stored rate meaningful.
func CreateRevision(ctx context.Context, tx pgx.Tx, actor string, effectiveAt time.Time,
	prices []Price, vendorKind func(vendor string) (string, bool)) (Revision, error) {
	normalized, currency, err := validatePrices(prices, vendorKind)
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
		"SELECT COALESCE(MAX(revision), 0) + 1 FROM olp_go.pricing_revisions").Scan(&number); err != nil {
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
	if _, err = tx.Exec(ctx, `INSERT INTO olp_go.pricing_revisions
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
		"SELECT btrim(currency) FROM olp_go.pricing_currency WHERE singleton").Scan(&configured)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx,
			"INSERT INTO olp_go.pricing_currency (singleton, currency) VALUES (true, $1)",
			currency); err != nil {
			return fmt.Errorf("store installation pricing currency: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read installation pricing currency: %w", err)
	}
	if configured != currency {
		return access.Invalid("prices",
			"Pricing must use the installation currency "+configured+".")
	}
	return nil
}

// insertPrice stores one rate. A provider-scoped override must reference a
// provider of the declared kind, or the override would never be selected and
// the operator would be left believing a rate is in force.
func insertPrice(ctx context.Context, tx pgx.Tx, revisionID string, price Price) error {
	if price.ProviderID != nil {
		var kind string
		err := tx.QueryRow(ctx, "SELECT kind FROM olp_go.providers WHERE id=$1", *price.ProviderID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != price.ProviderKind) {
			return access.Invalid("prices",
				"A pricing override must reference a provider of the declared kind.")
		}
		if err != nil {
			return fmt.Errorf("read pricing override provider: %w", err)
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp_go.prices
            (pricing_revision_id, provider_kind, provider_id, model, operation,
             input_per_million, cached_input_per_million, output_per_million,
             unit_price, currency, vendor_id)
        VALUES ($1, $2, $3, $4, $5, $6::text::numeric, $7::text::numeric, $8::text::numeric,
                $9::text::numeric, $10, $11)`,
		revisionID, price.ProviderKind, price.ProviderID, price.Model, price.Operation,
		price.InputPerMillion, price.CachedInputPerMillion, price.OutputPerMillion,
		price.UnitPrice, price.Currency, price.VendorID)
	if err != nil {
		return fmt.Errorf("store price: %w", err)
	}
	return nil
}

// validatePrices checks one revision as a whole and returns the normalized
// rates plus the revision currency.
func validatePrices(prices []Price, vendorKind func(vendor string) (string, bool)) ([]Price, string, error) {
	if len(prices) < 1 || len(prices) > MaxRevisionPrices {
		return nil, "", access.Invalid("prices",
			"A pricing revision must contain 1–"+strconv.Itoa(MaxRevisionPrices)+" entries.")
	}
	normalized := make([]Price, 0, len(prices))
	dimensions := make(map[string]bool, len(prices))
	currency := ""
	for _, price := range prices {
		entry, err := normalizePrice(price, vendorKind)
		if err != nil {
			return nil, "", err
		}
		if currency == "" {
			currency = entry.Currency
		} else if currency != entry.Currency {
			return nil, "", access.Invalid("prices", "A pricing revision cannot mix currencies.")
		}
		key := entry.ProviderKind + "\x00" + optional(entry.ProviderID) + "\x00" +
			optional(entry.VendorID) + "\x00" + entry.Model + "\x00" + entry.Operation
		if dimensions[key] {
			return nil, "", access.Invalid("prices",
				"A pricing revision cannot repeat the same scoped dimensions.")
		}
		dimensions[key] = true
		normalized = append(normalized, entry)
	}
	return normalized, currency, nil
}

// optional renders an absent scope distinctly from any value it could hold, so
// the duplicate check cannot confuse "any provider" with a provider named "".
func optional(value *string) string {
	if value == nil {
		return "\x01"
	}
	return *value
}

func normalizePrice(price Price, vendorKind func(vendor string) (string, bool)) (Price, error) {
	entry := price
	entry.Model = strings.TrimSpace(price.Model)
	entry.Currency = strings.ToUpper(strings.TrimSpace(price.Currency))
	if !validProviderKind(entry.ProviderKind) {
		return Price{}, access.Invalid("prices", "Use a supported provider kind.")
	}
	if !validOperation(entry.Operation) {
		return Price{}, access.Invalid("prices", "Use a supported pricing operation.")
	}
	if entry.VendorID != nil {
		vendor := strings.TrimSpace(*entry.VendorID)
		if vendorKind == nil {
			return Price{}, errors.New("pricing vendor catalogue is unavailable")
		}
		kind, known := vendorKind(vendor)
		if !known || kind != entry.ProviderKind {
			return Price{}, access.Invalid("prices", "The pricing vendor must match the connector.")
		}
		entry.VendorID = &vendor
	}
	if entry.ProviderID != nil {
		id, err := access.ParseUUID(*entry.ProviderID)
		if err != nil {
			return Price{}, access.Invalid("prices", "Use a valid provider identifier.")
		}
		entry.ProviderID = &id
	}
	if entry.Model == "" || len(entry.Currency) != 3 || !currencyLetters(entry.Currency) {
		return Price{}, access.Invalid("prices",
			"Pricing entries require a model and a three-letter ISO currency.")
	}
	if entry.InputPerMillion == nil && entry.OutputPerMillion == nil && entry.UnitPrice == nil {
		return Price{}, access.Invalid("prices", "Pricing entries require at least one rate.")
	}
	// A cached rate prices a share of the input count; without an input rate
	// there is nothing for it to discount.
	if entry.CachedInputPerMillion != nil && entry.InputPerMillion == nil {
		return Price{}, access.Invalid("prices",
			"A cached input rate requires an input rate to discount.")
	}
	for _, amount := range []**string{&entry.InputPerMillion, &entry.CachedInputPerMillion,
		&entry.OutputPerMillion, &entry.UnitPrice} {
		if *amount == nil {
			continue
		}
		value, err := normalizeDecimal(**amount)
		if err != nil {
			return Price{}, err
		}
		*amount = &value
	}
	return entry, nil
}

func currencyLetters(value string) bool {
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return value != ""
}

// normalizeDecimal accepts a non-negative decimal with at most 12 integer and
// 12 fractional digits, which is exactly what the numeric(24,12) columns hold.
// Anything else is refused rather than rounded.
func normalizeDecimal(value string) (string, error) {
	invalid := access.Invalid("prices",
		"Rates must be non-negative decimals with at most 12 integer and 12 fractional digits.")
	trimmed := strings.TrimSpace(value)
	integer, fraction, hasFraction := strings.Cut(trimmed, ".")
	if trimmed == "" || len(integer) < 1 || len(integer) > 12 || !priceDigits(integer) {
		return "", invalid
	}
	if hasFraction && (len(fraction) < 1 || len(fraction) > 12 || !priceDigits(fraction)) {
		return "", invalid
	}
	return trimmed, nil
}

func priceDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

const revisionColumns = `SELECT r.id::text, r.revision, r.effective_at, r.created_by::text,
        r.created_at, p.vendor_id, p.provider_kind, p.provider_id::text, p.model, p.operation,
        p.input_per_million::text, p.cached_input_per_million::text, p.output_per_million::text,
        p.unit_price::text, btrim(p.currency)
    FROM olp_go.pricing_revisions r LEFT JOIN olp_go.prices p ON p.pricing_revision_id = r.id
    WHERE r.id IN (SELECT id FROM olp_go.pricing_revisions
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
		var price Price
		var kind, model, operation, currency *string
		if err = rows.Scan(&revision.ID, &revision.Revision, &revision.EffectiveAt,
			&revision.CreatedBy, &revision.CreatedAt, &price.VendorID, &kind, &price.ProviderID,
			&model, &operation, &price.InputPerMillion, &price.CachedInputPerMillion,
			&price.OutputPerMillion, &price.UnitPrice, &currency); err != nil {
			return nil, nil, fmt.Errorf("list pricing revisions: %w", err)
		}
		if len(items) == 0 || items[len(items)-1].ID != revision.ID {
			revision.EffectiveAt = revision.EffectiveAt.UTC()
			revision.CreatedAt = revision.CreatedAt.UTC()
			revision.Prices = []Price{}
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

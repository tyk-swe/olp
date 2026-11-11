package pricebook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// Source formats: a price list document, or a signed reference catalog.
const (
	FormatPrices  = "prices"
	FormatCatalog = "catalog"
)

type Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Format is how the source document is read. A catalog source without
	// a URL reads the catalog this release ships.
	Format    string    `json:"format"`
	URL       *string   `json:"url"`
	Enabled   bool      `json:"enabled"`
	ETag      string    `json:"etag"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SourceSnapshot struct {
	ID         string          `json:"id"`
	SourceID   string          `json:"source_id"`
	SHA256     string          `json:"sha256"`
	Currency   string          `json:"currency"`
	PriceCount int             `json:"price_count"`
	FetchedAt  time.Time       `json:"fetched_at"`
	Document   json.RawMessage `json:"document"`
	// Catalog is the signed catalog the snapshot was mapped from, for a
	// catalog source.
	Catalog *SnapshotCatalog `json:"catalog"`
}

type SourceDiff struct {
	Added        []map[string]string `json:"added"`
	Removed      []map[string]string `json:"removed"`
	Changed      []map[string]string `json:"changed"`
	AddedCount   int                 `json:"added_count"`
	RemovedCount int                 `json:"removed_count"`
	ChangedCount int                 `json:"changed_count"`
}

type SourceRefresh struct {
	Source   Source         `json:"source"`
	Snapshot SourceSnapshot `json:"snapshot"`
	Diff     SourceDiff     `json:"diff"`
	// Skipped are catalog prices this build cannot store.
	Skipped []SkippedPrice `json:"skipped"`
}

const sourceFetchTimeout = 15 * time.Second

const maxSourceDocumentBytes = 4 << 20

type sourceDocument struct {
	Currency    string        `json:"currency"`
	EffectiveAt *time.Time    `json:"effective_at"`
	Prices      []rates.Price `json:"prices"`
	// Unrepresentable are the components a catalog lists that no price
	// holds; publishing a snapshot never prices them.
	Unrepresentable []UnrepresentablePrice `json:"unrepresentable,omitempty"`
}

const sourceReturning = `id::text, name, format, url, enabled, etag::text, created_by::text,
        created_at, updated_at`

const sourceColumns = `SELECT ` + sourceReturning + ` FROM olp.pricing_sources`

const snapshotReturning = `id::text, source_id::text, sha256, document, fetched_at,
        catalog_sha256, catalog_published_at, catalog_key_id`

const snapshotColumns = `SELECT ` + snapshotReturning + ` FROM olp.pricing_source_snapshots`

func scanSource(row pgx.Row) (Source, error) {
	var source Source
	err := row.Scan(&source.ID, &source.Name, &source.Format, &source.URL, &source.Enabled,
		&source.ETag, &source.CreatedBy, &source.CreatedAt, &source.UpdatedAt)
	source.CreatedAt = source.CreatedAt.UTC()
	source.UpdatedAt = source.UpdatedAt.UTC()
	return source, err
}

func scanSnapshot(row pgx.Row) (SourceSnapshot, error) {
	var snapshot SourceSnapshot
	var document []byte
	var catalogSHA256, catalogKeyID *string
	var catalogPublishedAt *time.Time
	err := row.Scan(&snapshot.ID, &snapshot.SourceID, &snapshot.SHA256,
		&document, &snapshot.FetchedAt, &catalogSHA256, &catalogPublishedAt, &catalogKeyID)
	if err != nil {
		return snapshot, err
	}
	if catalogSHA256 != nil && catalogPublishedAt != nil && catalogKeyID != nil {
		snapshot.Catalog = &SnapshotCatalog{SHA256: *catalogSHA256, PublishedAt: catalogPublishedAt.UTC().Format(time.RFC3339), KeyID: *catalogKeyID}
	}
	snapshot.FetchedAt = snapshot.FetchedAt.UTC()
	snapshot.Document = json.RawMessage(document)
	var doc sourceDocument
	if err = json.Unmarshal(document, &doc); err != nil {
		return snapshot, errors.New("stored pricing source snapshot is invalid")
	}
	snapshot.Currency = doc.Currency
	snapshot.PriceCount = len(doc.Prices)
	return snapshot, nil
}

func loadSource(ctx context.Context, q access.Queryer, id string) (Source, error) {
	return scanSource(q.QueryRow(ctx, sourceColumns+" WHERE id=$1", id))
}

type sourceInput struct {
	Name    string  `json:"name"`
	Format  string  `json:"format"`
	URL     *string `json:"url"`
	Enabled *bool   `json:"enabled"`
}

// fetchedSource is a fetched, validated source document and its canonical
// bytes, with the signed catalog it was mapped from, for a catalog source.
type fetchedSource struct {
	Document  sourceDocument
	Canonical []byte
	Catalog   *catalog.Signed
	Skipped   []SkippedPrice
}

// signatureURL is where a fetched catalog's detached signature lives: the
// catalog's URL with ".sig" appended to its path.
func signatureURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return "", errors.New("the catalog URL has no path")
	}
	u.Path += ".sig"
	u.RawPath = ""
	return u.String(), nil
}

func canonicalSourceDocument(doc sourceDocument) []byte {
	prices := slices.Clone(doc.Prices)
	slices.SortFunc(prices, func(a, b rates.Price) int {
		return strings.Compare(dimensionKey(a), dimensionKey(b))
	})
	canonical := sourceDocument{Currency: doc.Currency, EffectiveAt: doc.EffectiveAt, Prices: prices, Unrepresentable: doc.Unrepresentable}
	data, err := json.Marshal(canonical)
	if err != nil {
		return nil
	}
	return data
}

func readBounded(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("document exceeds the size limit")
	}
	return data, nil
}

// advanceCatalog records the newest catalog a source took, refusing an older
// one: a replayed signed catalog must never roll the source's prices back.
func advanceCatalog(ctx context.Context, tx pgx.Tx, sourceID string, published time.Time) error {
	var newest *time.Time
	if err := tx.QueryRow(ctx, "SELECT catalog_published_at FROM olp.pricing_sources WHERE id=$1 FOR UPDATE", sourceID).Scan(&newest); err != nil {
		return err
	}
	if newest != nil && published.Before(*newest) {
		return ErrCatalogOutdated
	}
	_, err := tx.Exec(ctx, "UPDATE olp.pricing_sources SET catalog_published_at=$2 WHERE id=$1", sourceID, published)
	return err
}

func dimensionSortKey(dimensions map[string]string) string {
	parts := make([]string, 0, len(dimensions))
	for _, key := range []string{"provider_kind", "provider_id", "vendor_id", "model", "operation"} {
		parts = append(parts, dimensions[key])
	}
	return strings.Join(parts, "\x00")
}

// SamePrice reports whether two prices carry the same currency and rates across
// every component, comparing decimals by numeric value.
func SamePrice(a, b rates.Price) bool {
	rates := [][2]*string{
		{a.InputPerMillion, b.InputPerMillion},
		{a.CachedInputPerMillion, b.CachedInputPerMillion},
		{a.OutputPerMillion, b.OutputPerMillion},
		{a.CacheWriteInputPerMillion, b.CacheWriteInputPerMillion},
		{a.CacheWrite5MInputPerMillion, b.CacheWrite5MInputPerMillion},
		{a.CacheWrite1HInputPerMillion, b.CacheWrite1HInputPerMillion},
		{a.AudioInputPerMillion, b.AudioInputPerMillion},
		{a.CachedAudioInputPerMillion, b.CachedAudioInputPerMillion},
		{a.AudioOutputPerMillion, b.AudioOutputPerMillion},
		{a.ImageInputPerMillion, b.ImageInputPerMillion},
		{a.CachedImageInputPerMillion, b.CachedImageInputPerMillion},
		{a.ImageOutputPerMillion, b.ImageOutputPerMillion},
		{a.UnitPrice, b.UnitPrice},
		{a.PerRequest, b.PerRequest},
		{a.BatchMultiplier, b.BatchMultiplier},
		{a.TrainingPerMillion, b.TrainingPerMillion},
		{a.StoragePerGiBDay, b.StoragePerGiBDay},
		{a.CacheStoragePerMillionTokenHour, b.CacheStoragePerMillionTokenHour},
	}
	for _, pair := range rates {
		if (pair[0] == nil) != (pair[1] == nil) {
			return false
		}
		if pair[0] != nil && !sameRate(*pair[0], *pair[1]) {
			return false
		}
	}
	return a.Currency == b.Currency && sameToolCallFees(a.ToolCalls, b.ToolCalls) &&
		sameToolCallFees(a.ContainerPerSession, b.ContainerPerSession) &&
		sameTiers(a.Tiers, b.Tiers) && sameWindows(a.Windows, b.Windows)
}

// sameToolCallFees compares two fee maps by tool and value.
func sameToolCallFees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for tool, fee := range a {
		other, ok := b[tool]
		if !ok || !sameRate(fee, other) {
			return false
		}
	}
	return true
}

// sameTiers compares two tier lists element by element.
func sameTiers(a, b []rates.PriceTier) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		left, right := a[index], b[index]
		if left.AboveInputTokens != right.AboveInputTokens {
			return false
		}
		rates := [][2]*string{
			{left.InputPerMillion, right.InputPerMillion},
			{left.CachedInputPerMillion, right.CachedInputPerMillion},
			{left.CacheWriteInputPerMillion, right.CacheWriteInputPerMillion},
			{left.CacheWrite5MInputPerMillion, right.CacheWrite5MInputPerMillion},
			{left.CacheWrite1HInputPerMillion, right.CacheWrite1HInputPerMillion},
			{left.OutputPerMillion, right.OutputPerMillion},
			{left.AudioInputPerMillion, right.AudioInputPerMillion},
			{left.CachedAudioInputPerMillion, right.CachedAudioInputPerMillion},
			{left.AudioOutputPerMillion, right.AudioOutputPerMillion},
			{left.ImageInputPerMillion, right.ImageInputPerMillion},
			{left.CachedImageInputPerMillion, right.CachedImageInputPerMillion},
			{left.ImageOutputPerMillion, right.ImageOutputPerMillion},
		}
		for _, pair := range rates {
			if (pair[0] == nil) != (pair[1] == nil) {
				return false
			}
			if pair[0] != nil && !sameRate(*pair[0], *pair[1]) {
				return false
			}
		}
	}
	return true
}

// sameWindows compares two window sets: the zone, and the periods as a set of
// day, start, end and multiplier.
func sameWindows(a, b *rates.PriceWindows) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil || a.TimeZone != b.TimeZone || len(a.Periods) != len(b.Periods) {
		return a == nil
	}
	key := func(p rates.PriceWindowPeriod) string {
		days := slices.Clone(p.Days)
		slices.Sort(days)
		var text strings.Builder
		for _, day := range days {
			text.WriteString(strconv.Itoa(day))
			text.WriteByte(',')
		}
		return text.String() + p.Start + "-" + p.End
	}
	rateAt := func(w *rates.PriceWindows, k string) *string {
		for index := range w.Periods {
			if key(w.Periods[index]) == k {
				return &w.Periods[index].Multiplier
			}
		}
		return nil
	}
	for _, period := range a.Periods {
		other := rateAt(b, key(period))
		if other == nil || !sameRate(period.Multiplier, *other) {
			return false
		}
	}
	return true
}

// sameRate compares two decimal rates by value: numeric(24,12) columns read back
// padded to twelve fractional digits while source text keeps its own scale.
func sameRate(a, b string) bool {
	x, errA := decimal.NewFromString(a)
	y, errB := decimal.NewFromString(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return x.Equal(y)
}

type publishInput struct {
	EffectiveAt *time.Time    `json:"effective_at"`
	Overrides   []rates.Price `json:"overrides"`
}

func mergeSourceOverrides(source, overrides []rates.Price, normalizer Normalizer) ([]rates.Price, error) {
	merged := map[string]rates.Price{}
	for _, price := range source {
		merged[dimensionKey(price)] = price
	}
	for _, override := range overrides {
		if isRemoval(override) {
			entry, err := normalizeRemovalDimensions(override, normalizer)
			if err != nil {
				return nil, err
			}
			delete(merged, dimensionKey(entry))
			continue
		}
		entry, err := normalizePrice(override, normalizer)
		if err != nil {
			return nil, err
		}
		merged[dimensionKey(entry)] = entry
	}
	prices := make([]rates.Price, 0, len(merged))
	for _, price := range merged {
		prices = append(prices, price)
	}
	slices.SortFunc(prices, func(a, b rates.Price) int {
		return strings.Compare(dimensionKey(a), dimensionKey(b))
	})
	return prices, nil
}

func normalizeRemovalDimensions(price rates.Price, normalizer Normalizer) (rates.Price, error) {
	entry := price
	entry.Model = strings.TrimSpace(entry.Model)
	entry.Currency = strings.ToUpper(strings.TrimSpace(entry.Currency))
	if !validProviderKind(entry.ProviderKind) || !usage.ValidOperation(entry.Operation) || entry.Model == "" {
		return rates.Price{}, invalid("overrides",
			"A removal override must name valid scoped dimensions.")
	}
	if entry.VendorID != nil {
		vendor := strings.TrimSpace(*entry.VendorID)
		kind, known := normalizer.vendorKind(vendor)
		if !known || kind != entry.ProviderKind {
			return rates.Price{}, invalid("overrides", "The pricing vendor must match the connector.")
		}
		entry.VendorID = &vendor
	}
	if entry.ProviderID != nil {
		id, err := access.ParseUUID(*entry.ProviderID)
		if err != nil {
			return rates.Price{}, invalid("overrides", "Use a valid provider identifier.")
		}
		entry.ProviderID = &id
	}
	return entry, nil
}

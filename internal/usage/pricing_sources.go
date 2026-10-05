package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/signing"
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
	Currency    string     `json:"currency"`
	EffectiveAt *time.Time `json:"effective_at"`
	Prices      []Price    `json:"prices"`
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

func (s *Server) listPricingSources(r *http.Request, p access.Principal) (access.Reply, error) {
	var err error
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(),
		sourceColumns+" WHERE id<$1::uuid ORDER BY id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []Source{}
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, source)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(source Source) string { return source.ID }), nil
}

func (s *Server) getPricingSource(r *http.Request, p access.Principal) (access.Reply, error) {
	var err error
	id, err := access.IDParam(r, "pricing_source_id")
	if err != nil {
		return access.Reply{}, err
	}
	source, err := loadSource(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(source, source.ETag), nil
}

func (s *Server) createPricingSource(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input sourceInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	principal, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if err = s.validateSourceInput(&input); err != nil {
		return access.Reply{}, err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	id, etag := access.NewID(), access.NewID()
	source, err := scanSource(tx.QueryRow(r.Context(),
		`INSERT INTO olp.pricing_sources (id, name, format, url, enabled, etag, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+sourceReturning,
		id, strings.TrimSpace(input.Name), input.Format, input.URL, enabled, etag, principal.UserID()))
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, ETag: source.ETag,
		Location: "/api/v1/pricing/sources/" + source.ID, Body: source}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "pricing_source.create",
		"pricing_source", source.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

// validateSourceInput normalizes a new source: a price list needs a URL, and
// a catalog source without one reads the catalog this release ships.
func (s *Server) validateSourceInput(input *sourceInput) error {
	if err := access.ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if input.Format == "" {
		input.Format = FormatPrices
	}
	if input.Format != FormatPrices && input.Format != FormatCatalog {
		return access.Invalid("format", "Use prices or catalog.")
	}
	if input.URL == nil {
		if input.Format == FormatPrices {
			return access.Invalid("url", "A price list source needs a URL.")
		}
		return nil
	}
	trimmed := strings.TrimSpace(*input.URL)
	input.URL = &trimmed
	return s.validateSourceURL(trimmed)
}

func (s *Server) validateSourceURL(raw string) error {
	if s.Egress == nil {
		return access.Fail(503, "egress_policy_unavailable", "Source fetching is not configured on this installation.")
	}
	if _, err := s.Egress.ValidateEndpoint(raw); err != nil {
		return access.Fail(422, "invalid_url", "The source URL is not permitted by egress policy.")
	}
	return nil
}

func (s *Server) updatePricingSource(r *http.Request, _ access.Principal) (access.Reply, error) {
	var patch map[string]json.RawMessage
	if err := access.Decode(r, &patch); err != nil {
		return access.Reply{}, err
	}
	if len(patch) == 0 {
		return access.Reply{}, access.Invalid("source", "Send at least one field to update.")
	}
	for field := range patch {
		if field == "format" {
			return access.Reply{}, access.Invalid(field, "A source keeps its format; create another source instead.")
		}
		if field != "name" && field != "url" && field != "enabled" {
			return access.Reply{}, access.Invalid(field, "Unknown pricing source field.")
		}
	}
	id, err := access.IDParam(r, "pricing_source_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	principal, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	source, err := scanSource(tx.QueryRow(r.Context(),
		sourceColumns+" WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, source.ETag); err != nil {
		return access.Reply{}, err
	}
	name, rawURL, enabled := source.Name, source.URL, source.Enabled
	if raw, ok := patch["name"]; ok {
		if err = json.Unmarshal(raw, &name); err != nil {
			return access.Reply{}, access.Invalid("name", "Use a non-empty source name.")
		}
		if err = access.ValidText("name", name, 100); err != nil {
			return access.Reply{}, err
		}
		name = strings.TrimSpace(name)
	}
	if raw, ok := patch["url"]; ok {
		if err = json.Unmarshal(raw, &rawURL); err != nil {
			return access.Reply{}, access.Invalid("url", "Use a valid source URL.")
		}
		if rawURL == nil {
			if source.Format == FormatPrices {
				return access.Reply{}, access.Invalid("url", "A price list source needs a URL.")
			}
		} else {
			trimmed := strings.TrimSpace(*rawURL)
			if err = s.validateSourceURL(trimmed); err != nil {
				return access.Reply{}, err
			}
			rawURL = &trimmed
		}
	}
	if raw, ok := patch["enabled"]; ok {
		if err = json.Unmarshal(raw, &enabled); err != nil {
			return access.Reply{}, access.Invalid("enabled", "Use true or false.")
		}
	}
	source, err = scanSource(tx.QueryRow(r.Context(),
		`UPDATE olp.pricing_sources SET name=$2, url=$3, enabled=$4, etag=$5, updated_at=now()
		 WHERE id=$1
		 RETURNING `+sourceReturning,
		id, name, rawURL, enabled, access.NewID()))
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "pricing_source.update",
		"pricing_source", source.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Detail(source, source.ETag))
}

// fetchedSource is a fetched, validated source document and its canonical
// bytes, with the signed catalog it was mapped from, for a catalog source.
type fetchedSource struct {
	Document  sourceDocument
	Canonical []byte
	Catalog   *SnapshotCatalog
	Published time.Time
	Skipped   []SkippedPrice
}

func (s *Server) fetchSource(ctx context.Context, source Source) (fetchedSource, error) {
	if source.Format == FormatCatalog {
		return s.fetchCatalog(ctx, source.URL)
	}
	if source.URL == nil {
		return fetchedSource{}, errors.New("stored price list source has no URL")
	}
	return s.fetchPriceDocument(ctx, *source.URL)
}

// fetchBounded GETs a source document through the egress policy. A price
// list must declare JSON; a signed catalog may come as anything, because its
// signature, not its media type, establishes what it is.
func (s *Server) fetchBounded(ctx context.Context, rawURL string, limit int64, requireJSON bool) ([]byte, error) {
	if s.Egress == nil {
		return nil, access.Fail(503, "egress_policy_unavailable",
			"Source fetching is not configured on this installation.")
	}
	client := s.Egress.Client(sourceFetchTimeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, access.Fail(422, "invalid_url", "The source URL is not permitted by egress policy.")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, access.Fail(502, "source_fetch_failed", "The pricing source could not be fetched.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, access.Fail(502, "source_fetch_failed", "The pricing source did not answer successfully.")
	}
	if requireJSON {
		mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			return nil, access.Fail(415, "invalid_source_document",
				"The pricing source did not return a JSON document.")
		}
	}
	body, err := readBounded(response.Body, limit)
	if err != nil {
		return nil, access.Fail(413, "source_document_too_large",
			fmt.Sprintf("The pricing source document exceeds the %d MiB limit.", limit>>20))
	}
	return body, nil
}

func (s *Server) fetchPriceDocument(ctx context.Context, rawURL string) (fetchedSource, error) {
	body, err := s.fetchBounded(ctx, rawURL, maxSourceDocumentBytes, true)
	if err != nil {
		return fetchedSource{}, err
	}
	var doc sourceDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var trailing any
	if err := decoder.Decode(&doc); err != nil || doc.Prices == nil ||
		decoder.Decode(&trailing) != io.EOF {
		return fetchedSource{}, access.Fail(422, "invalid_source_document",
			"The pricing source document is not a valid pricing catalogue.")
	}
	doc.Currency = strings.ToUpper(strings.TrimSpace(doc.Currency))
	if len(doc.Currency) != 3 || !currencyLetters(doc.Currency) {
		return fetchedSource{}, access.Fail(422, "invalid_source_document",
			"The pricing source document does not declare a valid currency.")
	}
	return s.validatedSource(doc, nil, time.Time{}, nil)
}

// fetchCatalog reads a signed reference catalog: the one this release ships,
// or one fetched with its detached signature at the same URL plus ".sig".
// The signature must verify against the keys this build trusts before any
// price in it is read, and a catalog older than the one this release ships
// is refused.
func (s *Server) fetchCatalog(ctx context.Context, rawURL *string) (fetchedSource, error) {
	signed := s.Catalog
	if rawURL != nil {
		document, err := s.fetchBounded(ctx, *rawURL, catalog.MaxBytes, false)
		if err != nil {
			return fetchedSource{}, err
		}
		signatureAt, err := signatureURL(*rawURL)
		if err != nil {
			return fetchedSource{}, access.Fail(422, "invalid_url", "The source URL is not permitted by egress policy.")
		}
		signature, err := s.fetchBounded(ctx, signatureAt, signing.MaxSignatureFile, false)
		if err != nil {
			return fetchedSource{}, err
		}
		signed, err = catalog.Load(document, signature, s.CatalogKeys)
		switch {
		case errors.Is(err, signing.ErrInvalidSignature) || errors.Is(err, signing.ErrUnknownKey) || errors.Is(err, signing.ErrMalformedSignature):
			return fetchedSource{}, access.Fail(422, "catalog_signature_invalid",
				"The catalog's signature does not verify against the keys this release trusts.")
		case errors.Is(err, catalog.ErrUnsupportedVersion):
			return fetchedSource{}, access.Fail(422, "catalog_unsupported_version",
				"The catalog is in a format this release cannot read.")
		case err != nil:
			return fetchedSource{}, access.Fail(422, "invalid_source_document", "The catalog is not a valid reference catalog.")
		}
	}
	if signed == nil {
		return fetchedSource{}, access.Fail(503, "reference_catalog_unavailable",
			"This process holds no verified reference catalog.")
	}
	if s.Catalog != nil && signed.Catalog.PublishedAt.Before(s.Catalog.Catalog.PublishedAt) {
		return fetchedSource{}, access.Fail(422, "catalog_outdated",
			"The catalog is older than the one this release ships.")
	}
	doc, skipped := catalogDocument(signed.Catalog, s.VendorKind)
	provenance := &SnapshotCatalog{SHA256: signed.SHA256, PublishedAt: signed.Catalog.PublishedAt.UTC().Format(time.RFC3339), KeyID: signed.KeyID}
	return s.validatedSource(doc, provenance, signed.Catalog.PublishedAt, skipped)
}

func (s *Server) validatedSource(doc sourceDocument, provenance *SnapshotCatalog, published time.Time, skipped []SkippedPrice) (fetchedSource, error) {
	normalized, currency, err := validatePrices(doc.Prices, s.VendorKind)
	if err != nil {
		return fetchedSource{}, err
	}
	doc.Prices = normalized
	if currency != doc.Currency {
		return fetchedSource{}, access.Fail(422, "invalid_source_document",
			"The pricing source currency does not match its prices.")
	}
	if skipped == nil {
		skipped = []SkippedPrice{}
	}
	return fetchedSource{Document: doc, Canonical: canonicalSourceDocument(doc), Catalog: provenance, Published: published, Skipped: skipped}, nil
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
	slices.SortFunc(prices, func(a, b Price) int {
		return strings.Compare(a.dimensionKey(), b.dimensionKey())
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

func (s *Server) refreshPricingSource(r *http.Request, principal access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "pricing_source_id")
	if err != nil {
		return access.Reply{}, err
	}
	source, err := loadSource(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	fetched, err := s.fetchSource(r.Context(), source)
	if err != nil {
		return access.Reply{}, err
	}
	digest := sha256.Sum256(fetched.Canonical)
	sum := hex.EncodeToString(digest[:])
	diff, err := s.diffLatest(r.Context(), fetched.Document.Prices)
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	// The fetch ran without the installation lock, so authority is decided
	// again under it before the snapshot and audit record are written.
	principal, err = s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var catalogSHA256, catalogKeyID *string
	var catalogPublishedAt *time.Time
	if fetched.Catalog != nil {
		if err = advanceCatalog(r.Context(), tx, source.ID, fetched.Published); err != nil {
			return access.Reply{}, err
		}
		catalogSHA256, catalogKeyID, catalogPublishedAt = &fetched.Catalog.SHA256, &fetched.Catalog.KeyID, &fetched.Published
	}
	// A snapshot is immutable: refetching identical prices keeps the first
	// snapshot and the catalog provenance it was taken from.
	snapshot, err := scanSnapshot(tx.QueryRow(r.Context(),
		`INSERT INTO olp.pricing_source_snapshots (id, source_id, sha256, document, catalog_sha256, catalog_published_at, catalog_key_id)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		 ON CONFLICT (source_id, sha256) DO UPDATE SET source_id = EXCLUDED.source_id
		 RETURNING `+snapshotReturning,
		access.NewID(), source.ID, sum, string(fetched.Canonical), catalogSHA256, catalogPublishedAt, catalogKeyID))
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "pricing_source.refresh",
		"pricing_source", source.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	refresh := SourceRefresh{Source: source, Snapshot: snapshot, Diff: diff, Skipped: fetched.Skipped}
	return access.Commit(r, tx, access.OK(refresh))
}

// advanceCatalog records the newest catalog a source took, refusing an older
// one: a replayed signed catalog must never roll the source's prices back.
func advanceCatalog(ctx context.Context, tx pgx.Tx, sourceID string, published time.Time) error {
	var newest *time.Time
	if err := tx.QueryRow(ctx, "SELECT catalog_published_at FROM olp.pricing_sources WHERE id=$1 FOR UPDATE", sourceID).Scan(&newest); err != nil {
		return err
	}
	if newest != nil && published.Before(*newest) {
		return access.Fail(422, "catalog_outdated", "The catalog is older than one this source already took.")
	}
	_, err := tx.Exec(ctx, "UPDATE olp.pricing_sources SET catalog_published_at=$2 WHERE id=$1", sourceID, published)
	return err
}

func (s *Server) diffLatest(ctx context.Context, candidate []Price) (SourceDiff, error) {
	revisions, _, err := ListRevisions(ctx, s.Access.Pool, nil, 1)
	if err != nil {
		return SourceDiff{}, err
	}
	current := map[string]Price{}
	if len(revisions) > 0 {
		for _, price := range revisions[0].Prices {
			current[price.dimensionKey()] = price
		}
	}
	incoming := map[string]Price{}
	for _, price := range candidate {
		incoming[price.dimensionKey()] = price
	}
	diff := SourceDiff{Added: []map[string]string{}, Removed: []map[string]string{}, Changed: []map[string]string{}}
	for key, price := range incoming {
		existing, ok := current[key]
		switch {
		case !ok:
			diff.Added = append(diff.Added, price.dimensions())
		case !samePrice(existing, price):
			diff.Changed = append(diff.Changed, price.dimensions())
		}
	}
	for key, price := range current {
		if _, ok := incoming[key]; !ok {
			diff.Removed = append(diff.Removed, price.dimensions())
		}
	}
	sortDimensions := func(list []map[string]string) {
		slices.SortFunc(list, func(a, b map[string]string) int {
			return strings.Compare(dimensionSortKey(a), dimensionSortKey(b))
		})
	}
	sortDimensions(diff.Added)
	sortDimensions(diff.Removed)
	sortDimensions(diff.Changed)
	diff.AddedCount, diff.RemovedCount, diff.ChangedCount = len(diff.Added), len(diff.Removed), len(diff.Changed)
	return diff, nil
}

func dimensionSortKey(dimensions map[string]string) string {
	parts := make([]string, 0, len(dimensions))
	for _, key := range []string{"provider_kind", "provider_id", "vendor_id", "model", "operation"} {
		parts = append(parts, dimensions[key])
	}
	return strings.Join(parts, "\x00")
}

func samePrice(a, b Price) bool {
	rates := [][2]*string{
		{a.InputPerMillion, b.InputPerMillion},
		{a.CachedInputPerMillion, b.CachedInputPerMillion},
		{a.OutputPerMillion, b.OutputPerMillion},
		{a.CacheWriteInputPerMillion, b.CacheWriteInputPerMillion},
		{a.CacheWrite5MInputPerMillion, b.CacheWrite5MInputPerMillion},
		{a.CacheWrite1HInputPerMillion, b.CacheWrite1HInputPerMillion},
		{a.UnitPrice, b.UnitPrice},
	}
	for _, pair := range rates {
		if (pair[0] == nil) != (pair[1] == nil) {
			return false
		}
		if pair[0] != nil && !sameRate(*pair[0], *pair[1]) {
			return false
		}
	}
	return a.Currency == b.Currency
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

func (s *Server) listPricingSourceSnapshots(r *http.Request, p access.Principal) (access.Reply, error) {
	var err error
	id, err := access.IDParam(r, "pricing_source_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = loadSource(r.Context(), s.Access.Pool, id); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(),
		snapshotColumns+" WHERE source_id=$1 AND id<$2::uuid ORDER BY id DESC LIMIT $3",
		id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []SourceSnapshot{}
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, snapshot)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(snapshot SourceSnapshot) string { return snapshot.ID }), nil
}

type publishInput struct {
	EffectiveAt *time.Time `json:"effective_at"`
	Overrides   []Price    `json:"overrides"`
}

func (s *Server) publishPricingSourceSnapshot(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input publishInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "pricing_source_snapshot_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	principal, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	snapshot, err := scanSnapshot(tx.QueryRow(r.Context(), snapshotColumns+" WHERE id=$1", id))
	if err != nil {
		return access.Reply{}, err
	}
	var doc sourceDocument
	if err = json.Unmarshal(snapshot.Document, &doc); err != nil {
		return access.Reply{}, errors.New("stored pricing source snapshot is invalid")
	}
	merged, err := mergeSourceOverrides(doc.Prices, input.Overrides, s.VendorKind)
	if err != nil {
		return access.Reply{}, err
	}
	_, currency, err := validatePrices(merged, s.VendorKind)
	if err != nil {
		return access.Reply{}, err
	}
	if currency != doc.Currency {
		return access.Reply{}, access.Fail(422, "invalid_price_set",
			"The merged price set must keep the source currency.")
	}
	effectiveAt := time.Now()
	if input.EffectiveAt != nil {
		effectiveAt = input.EffectiveAt.UTC()
		if effectiveAt.Before(time.Now()) {
			return access.Reply{}, access.Invalid("effective_at",
				"Choose an effective time now or in the future.")
		}
	}
	revision, err := CreateRevision(r.Context(), tx, principal.UserID(), effectiveAt, merged, s.VendorKind)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(),
		"UPDATE olp.pricing_revisions SET source_snapshot_id=$1 WHERE id=$2",
		snapshot.ID, revision.ID); err != nil {
		return access.Reply{}, fmt.Errorf("record pricing provenance: %w", err)
	}
	revision.SourceSnapshotID = &snapshot.ID
	result := access.Reply{Status: 201, Body: revision}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "pricing_source.publish",
		"pricing_source_snapshot", snapshot.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func mergeSourceOverrides(source, overrides []Price, vendorKind func(vendor string) (string, bool)) ([]Price, error) {
	merged := map[string]Price{}
	for _, price := range source {
		merged[price.dimensionKey()] = price
	}
	for _, override := range overrides {
		if override.isRemoval() {
			entry, err := normalizeRemovalDimensions(override, vendorKind)
			if err != nil {
				return nil, err
			}
			delete(merged, entry.dimensionKey())
			continue
		}
		entry, err := normalizePrice(override, vendorKind)
		if err != nil {
			return nil, err
		}
		merged[entry.dimensionKey()] = entry
	}
	prices := make([]Price, 0, len(merged))
	for _, price := range merged {
		prices = append(prices, price)
	}
	slices.SortFunc(prices, func(a, b Price) int {
		return strings.Compare(a.dimensionKey(), b.dimensionKey())
	})
	return prices, nil
}

func normalizeRemovalDimensions(price Price, vendorKind func(vendor string) (string, bool)) (Price, error) {
	entry := price
	entry.Model = strings.TrimSpace(entry.Model)
	entry.Currency = strings.ToUpper(strings.TrimSpace(entry.Currency))
	if !validProviderKind(entry.ProviderKind) || !validOperation(entry.Operation) || entry.Model == "" {
		return Price{}, access.Invalid("overrides",
			"A removal override must name valid scoped dimensions.")
	}
	if entry.VendorID != nil {
		vendor := strings.TrimSpace(*entry.VendorID)
		if vendorKind == nil {
			return Price{}, errors.New("pricing vendor catalogue is unavailable")
		}
		kind, known := vendorKind(vendor)
		if !known || kind != entry.ProviderKind {
			return Price{}, access.Invalid("overrides", "The pricing vendor must match the connector.")
		}
		entry.VendorID = &vendor
	}
	if entry.ProviderID != nil {
		id, err := access.ParseUUID(*entry.ProviderID)
		if err != nil {
			return Price{}, access.Invalid("overrides", "Use a valid provider identifier.")
		}
		entry.ProviderID = &id
	}
	return entry, nil
}

func (p Price) isRemoval() bool {
	return p.InputPerMillion == nil && p.CachedInputPerMillion == nil &&
		p.OutputPerMillion == nil && p.CacheWriteInputPerMillion == nil &&
		p.CacheWrite5MInputPerMillion == nil && p.CacheWrite1HInputPerMillion == nil &&
		p.UnitPrice == nil
}

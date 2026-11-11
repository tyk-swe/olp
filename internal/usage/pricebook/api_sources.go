package pricebook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func (s *API) listPricingSources(r *http.Request, p access.Principal) (access.Reply, error) {
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

func (s *API) getPricingSource(r *http.Request, p access.Principal) (access.Reply, error) {
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

func (s *API) createPricingSource(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input sourceInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
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
	return result, tx.Commit(r.Context())
}

// validateSourceInput normalizes a new source: a price list needs a URL, and
// a catalog source without one reads the catalog this release ships.
func (s *API) validateSourceInput(input *sourceInput) error {
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

func (s *API) validateSourceURL(raw string) error {
	if s.Egress == nil {
		return access.Fail(503, "egress_policy_unavailable", "Source fetching is not configured on this installation.")
	}
	if _, err := s.Egress.ValidateEndpoint(raw); err != nil {
		return access.Fail(422, "invalid_url", "The source URL is not permitted by egress policy.")
	}
	return nil
}

func (s *API) updatePricingSource(r *http.Request, _ access.Principal) (access.Reply, error) {
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
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
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
	return access.Detail(source, source.ETag), tx.Commit(r.Context())
}

func (s *API) refreshPricingSource(r *http.Request, _ access.Principal) (access.Reply, error) {
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
	// The fetch ran without the installation lock, so authority is decided
	// again under it before the snapshot and audit record are written.
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	var catalogSHA256, catalogKeyID *string
	var catalogPublishedAt *time.Time
	if signed := fetched.Catalog; signed != nil {
		if err = advanceCatalog(r.Context(), tx, source.ID, signed.Catalog.PublishedAt); err != nil {
			return access.Reply{}, err
		}
		catalogSHA256, catalogKeyID, catalogPublishedAt = &signed.SHA256, &signed.KeyID, &signed.Catalog.PublishedAt
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
	return access.OK(refresh), tx.Commit(r.Context())
}

func (s *API) diffLatest(ctx context.Context, candidate []rates.Price) (SourceDiff, error) {
	revisions, _, err := ListRevisions(ctx, s.Access.Pool, nil, 1)
	if err != nil {
		return SourceDiff{}, err
	}
	current := map[string]rates.Price{}
	if len(revisions) > 0 {
		for _, price := range revisions[0].Prices {
			current[dimensionKey(price)] = price
		}
	}
	incoming := map[string]rates.Price{}
	for _, price := range candidate {
		incoming[dimensionKey(price)] = price
	}
	diff := SourceDiff{Added: []map[string]string{}, Removed: []map[string]string{}, Changed: []map[string]string{}}
	for key, price := range incoming {
		existing, ok := current[key]
		switch {
		case !ok:
			diff.Added = append(diff.Added, priceDimensions(price))
		case !SamePrice(existing, price):
			diff.Changed = append(diff.Changed, priceDimensions(price))
		}
	}
	for key, price := range current {
		if _, ok := incoming[key]; !ok {
			diff.Removed = append(diff.Removed, priceDimensions(price))
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

func (s *API) listPricingSourceSnapshots(r *http.Request, p access.Principal) (access.Reply, error) {
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

func (s *API) publishPricingSourceSnapshot(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input publishInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "pricing_source_snapshot_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	snapshot, err := scanSnapshot(tx.QueryRow(r.Context(), snapshotColumns+" WHERE id=$1", id))
	if err != nil {
		return access.Reply{}, err
	}
	var doc sourceDocument
	if err = json.Unmarshal(snapshot.Document, &doc); err != nil {
		return access.Reply{}, errors.New("stored pricing source snapshot is invalid")
	}
	merged, err := mergeSourceOverrides(doc.Prices, input.Overrides, s.Normalizer)
	if err != nil {
		return access.Reply{}, err
	}
	_, currency, err := validatePrices(merged, s.Normalizer)
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
	revision, err := CreateRevision(r.Context(), tx, principal.UserID(), effectiveAt, merged, s.Normalizer)
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
	return result, tx.Commit(r.Context())
}

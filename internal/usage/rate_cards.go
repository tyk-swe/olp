package usage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
)

// RateCardOwner identifies who a card charges: the installation, one
// organization, or one project.
type RateCardOwner struct {
	Kind           string  `json:"kind"`
	OrganizationID *string `json:"organization_id,omitempty"`
	Organization   *string `json:"organization,omitempty"`
	ProjectID      *string `json:"project_id,omitempty"`
	Project        *string `json:"project,omitempty"`
}

// RateCard is one owner's charge terms: a card accumulates immutable
// revisions over time, the newest in force when an attempt is observed.
type RateCard struct {
	ID        string         `json:"id"`
	Owner     RateCardOwner  `json:"owner"`
	Revisions []CardRevision `json:"revisions,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// CardRevision is one immutable revision: a required markup plus optional
// explicit rates in the price-entry shape.
type CardRevision struct {
	ID          string    `json:"id"`
	RateCardID  string    `json:"rate_card_id"`
	Revision    int       `json:"revision"`
	Markup      string    `json:"markup"`
	EffectiveAt time.Time `json:"effective_at"`
	Prices      []Price   `json:"prices"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
}

type rateCardInput struct {
	Owner struct {
		Kind string  `json:"kind"`
		ID   *string `json:"id"`
	} `json:"owner"`
}

type cardRevisionInput struct {
	EffectiveAt *time.Time `json:"effective_at"`
	Markup      *string    `json:"markup"`
	Prices      []Price    `json:"prices"`
}

// cardColumns joins each card to its owner names for display.
const cardColumns = `SELECT c.id::text, c.organization_id::text, o.name, c.project_id::text, p.name, c.created_at
    FROM olp.rate_cards c
    LEFT JOIN olp.organizations o ON o.id = c.organization_id
    LEFT JOIN olp.projects p ON p.id = c.project_id`

func scanCard(row interface{ Scan(...any) error }) (RateCard, error) {
	var c RateCard
	var organizationID, organization, projectID, project *string
	err := row.Scan(&c.ID, &organizationID, &organization, &projectID, &project, &c.CreatedAt)
	c.Owner.Kind = "installation"
	switch {
	case projectID != nil:
		c.Owner.Kind = "project"
		c.Owner.ProjectID, c.Owner.Project = projectID, project
	case organizationID != nil:
		c.Owner.Kind = "organization"
		c.Owner.OrganizationID, c.Owner.Organization = organizationID, organization
	}
	return c, err
}

// cardPrice validates a card's explicit rates with the price-entry rules,
// refusing the connection scope a card cannot have.
func cardPrices(ctx context.Context, tx pgx.Tx, prices []Price, currency string,
	vendorKind func(vendor string) (string, bool)) ([]Price, error) {
	if len(prices) == 0 {
		return nil, nil
	}
	for i := range prices {
		if prices[i].ProviderID != nil {
			return nil, access.Invalid("prices", "Rate card prices cannot scope a provider.")
		}
		if declared := strings.TrimSpace(prices[i].Currency); declared != "" &&
			!strings.EqualFold(declared, currency) {
			return nil, access.Invalid("prices",
				"Rate card prices keep the installation currency.")
		}
		prices[i].Currency = currency
	}
	normalized, _, err := validatePrices(prices, vendorKind)
	if err != nil {
		return nil, err
	}
	for _, price := range normalized {
		if price.Windows != nil {
			var local time.Time
			if err := tx.QueryRow(ctx, "SELECT now() AT TIME ZONE $1", price.Windows.TimeZone).Scan(&local); err != nil {
				return nil, access.Invalid("prices", "The pricing window time zone is unknown.")
			}
		}
	}
	return normalized, nil
}

// insertCardPrice stores one explicit card rate; currency is the
// installation's.
func insertCardPrice(ctx context.Context, tx pgx.Tx, revisionID string, price Price) error {
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
	_, err = tx.Exec(ctx, `INSERT INTO olp.rate_card_prices
            (rate_card_revision_id, provider_kind, vendor_id, model, operation,
             input_per_million, cached_input_per_million, cache_write_input_per_million,
             cache_write_5m_input_per_million, cache_write_1h_input_per_million,
             output_per_million, audio_input_per_million, cached_audio_input_per_million,
             audio_output_per_million, image_input_per_million, cached_image_input_per_million,
             image_output_per_million, unit_price, per_request, batch_multiplier,
             tool_calls, tiers, windows, currency, training_per_million, storage_per_gib_day, cache_storage_per_million_token_hour, container_per_session)
        VALUES ($1::uuid, $2, $3, $4, $5, $6::text::numeric, $7::text::numeric, $8::text::numeric,
                $9::text::numeric, $10::text::numeric, $11::text::numeric, $12::text::numeric,
                $13::text::numeric, $14::text::numeric, $15::text::numeric, $16::text::numeric,
                $17::text::numeric, $18::text::numeric, $19::text::numeric, $20::text::numeric,
                $21::jsonb, $22::jsonb, $23::jsonb, $24, $25::text::numeric, $26::text::numeric, $27::text::numeric, $28::jsonb)`,
		revisionID, price.ProviderKind, price.VendorID, price.Model, price.Operation,
		price.InputPerMillion, price.CachedInputPerMillion, price.CacheWriteInputPerMillion,
		price.CacheWrite5MInputPerMillion, price.CacheWrite1HInputPerMillion,
		price.OutputPerMillion, price.AudioInputPerMillion, price.CachedAudioInputPerMillion,
		price.AudioOutputPerMillion, price.ImageInputPerMillion, price.CachedImageInputPerMillion,
		price.ImageOutputPerMillion, price.UnitPrice, price.PerRequest, price.BatchMultiplier,
		toolCalls, tiers, windows, price.Currency, price.TrainingPerMillion, price.StoragePerGiBDay, price.CacheStoragePerMillionTokenHour, optionalJSON(price.ContainerPerSession))
	if err != nil {
		return fmt.Errorf("store rate card price: %w", err)
	}
	return nil
}

const cardRevisionColumns = `SELECT r.id::text, r.rate_card_id::text, r.revision, r.markup::text,
        r.effective_at, r.created_at, r.created_by::text
    FROM olp.rate_card_revisions r`

func scanCardRevision(row interface{ Scan(...any) error }) (CardRevision, error) {
	var r CardRevision
	err := row.Scan(&r.ID, &r.RateCardID, &r.Revision, &r.Markup, &r.EffectiveAt, &r.CreatedAt, &r.CreatedBy)
	return r, err
}

func loadCardPrices(ctx context.Context, q access.Queryer, revisionID string) ([]Price, error) {
	byRevision, err := LoadCardPricesByRevisions(ctx, q, []string{revisionID})
	if err != nil {
		return nil, err
	}
	prices := byRevision[revisionID]
	if prices == nil {
		return []Price{}, nil
	}
	return prices, nil
}

// LoadCardPricesByRevisions reads the explicit rates of each named card
// revision; the configuration exporter and revision listings share it.
func LoadCardPricesByRevisions(ctx context.Context, q access.Queryer, revisionIDs []string) (map[string][]Price, error) {
	if len(revisionIDs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT rate_card_revision_id::text, provider_kind, vendor_id, model, operation,
        input_per_million::text, cached_input_per_million::text, cache_write_input_per_million::text,
        cache_write_5m_input_per_million::text, cache_write_1h_input_per_million::text,
        output_per_million::text, audio_input_per_million::text, cached_audio_input_per_million::text,
        audio_output_per_million::text, image_input_per_million::text, cached_image_input_per_million::text,
        image_output_per_million::text, unit_price::text, per_request::text, batch_multiplier::text,
        tool_calls::text, tiers::text, windows::text, btrim(currency), training_per_million::text, storage_per_gib_day::text, cache_storage_per_million_token_hour::text, container_per_session::text
    FROM olp.rate_card_prices
    WHERE rate_card_revision_id = ANY($1::uuid[])
    ORDER BY rate_card_revision_id, provider_kind, vendor_id, model, operation`, revisionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRevision := make(map[string][]Price, len(revisionIDs))
	for rows.Next() {
		var revisionID string
		var p Price
		var toolCalls, tiers, windows, containerSessions *string
		if err = rows.Scan(&revisionID, &p.ProviderKind, &p.VendorID, &p.Model, &p.Operation,
			&p.InputPerMillion, &p.CachedInputPerMillion, &p.CacheWriteInputPerMillion,
			&p.CacheWrite5MInputPerMillion, &p.CacheWrite1HInputPerMillion,
			&p.OutputPerMillion, &p.AudioInputPerMillion, &p.CachedAudioInputPerMillion,
			&p.AudioOutputPerMillion, &p.ImageInputPerMillion, &p.CachedImageInputPerMillion,
			&p.ImageOutputPerMillion, &p.UnitPrice, &p.PerRequest, &p.BatchMultiplier,
			&toolCalls, &tiers, &windows, &p.Currency, &p.TrainingPerMillion, &p.StoragePerGiBDay, &p.CacheStoragePerMillionTokenHour, &containerSessions); err != nil {
			return nil, err
		}
		if err = decodeContainerRates(&p, containerSessions); err != nil {
			return nil, err
		}
		if err = unmarshalPriceComponents(&p, toolCalls, tiers, windows, "rate card"); err != nil {
			return nil, err
		}
		byRevision[revisionID] = append(byRevision[revisionID], p)
	}
	return byRevision, rows.Err()
}

// CreateCardRevision stores one validated revision inside the caller's
// transaction, normalizing the markup and prices as the API does. Prices
// carry the installation currency.
func CreateCardRevision(ctx context.Context, tx pgx.Tx, actor, cardID, markup string,
	effectiveAt time.Time, prices []Price, currency string,
	vendorKind func(vendor string) (string, bool)) (CardRevision, error) {
	normalizedMarkup, err := normalizeDecimal(markup)
	if err != nil {
		return CardRevision{}, access.Invalid("markup", "Use a non-negative decimal.")
	}
	normalized, err := cardPrices(ctx, tx, prices, currency, vendorKind)
	if err != nil {
		return CardRevision{}, err
	}
	if len(normalized) > 0 {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(pricingLockID)); err != nil {
			return CardRevision{}, fmt.Errorf("lock pricing currency: %w", err)
		}
		if err := reserveCurrency(ctx, tx, currency); err != nil {
			return CardRevision{}, err
		}
	}
	return createCardRevision(ctx, tx, actor, cardID, normalizedMarkup, effectiveAt, normalized)
}

// createCardRevision stores one revision inside the caller's transaction.
func createCardRevision(ctx context.Context, tx pgx.Tx, actor, cardID, markup string,
	effectiveAt time.Time, prices []Price) (CardRevision, error) {
	var revision CardRevision
	// Serialize revision allocation on the parent, including its first revision.
	if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.rate_cards WHERE id=$1::uuid FOR UPDATE", cardID).Scan(&cardID); err != nil {
		return revision, fmt.Errorf("lock rate card: %w", err)
	}
	err := tx.QueryRow(ctx, `INSERT INTO olp.rate_card_revisions
            (id, rate_card_id, revision, markup, effective_at, created_by)
        VALUES ($1::uuid, $2::uuid,
                (SELECT COALESCE(MAX(revision),0)+1 FROM olp.rate_card_revisions WHERE rate_card_id=$2::uuid),
                $3::text::numeric, $4, $5::uuid)
        RETURNING id::text, rate_card_id::text, revision`,
		access.NewID(), cardID, markup, effectiveAt, actor).
		Scan(&revision.ID, &revision.RateCardID, &revision.Revision)
	if err != nil {
		return revision, fmt.Errorf("store rate card revision: %w", err)
	}
	for _, price := range prices {
		if err = insertCardPrice(ctx, tx, revision.ID, price); err != nil {
			return revision, err
		}
	}
	revision, err = scanCardRevision(tx.QueryRow(ctx, cardRevisionColumns+" WHERE r.id=$1::uuid", revision.ID))
	if err != nil {
		return revision, fmt.Errorf("store rate card revision: %w", err)
	}
	revision.Prices, err = loadCardPrices(ctx, tx, revision.ID)
	if err != nil {
		return revision, fmt.Errorf("store rate card revision: %w", err)
	}
	return revision, nil
}

func (s *Server) listRateCards(r *http.Request, _ access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(),
		cardColumns+" WHERE c.id<$1::uuid ORDER BY c.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []RateCard{}
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, card)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	cardIDs := make([]string, len(items))
	for i := range items {
		cardIDs[i] = items[i].ID
	}
	summaries, err := loadCardRevisionSummariesByCards(r.Context(), s.Access.Pool, cardIDs)
	if err != nil {
		return access.Reply{}, err
	}
	for i := range items {
		items[i].Revisions = summaries[items[i].ID]
		if items[i].Revisions == nil {
			items[i].Revisions = []CardRevision{}
		}
	}
	return access.ListReplyBy(items, page, func(c RateCard) string { return c.ID }), nil
}

// loadCardRevisionSummaries lists a card's in-force and latest revisions
// without their price rows.
func loadCardRevisionSummaries(ctx context.Context, q access.Queryer, cardID string) ([]CardRevision, error) {
	summaries, err := loadCardRevisionSummariesByCards(ctx, q, []string{cardID})
	if err != nil {
		return nil, err
	}
	if revisions := summaries[cardID]; revisions != nil {
		return revisions, nil
	}
	return []CardRevision{}, nil
}

func loadCardRevisionSummariesByCards(ctx context.Context, q access.Queryer, cardIDs []string) (map[string][]CardRevision, error) {
	if len(cardIDs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, cardRevisionColumns+
		` WHERE r.rate_card_id = ANY($1::uuid[])
		  AND (r.revision=(SELECT MAX(revision) FROM olp.rate_card_revisions WHERE rate_card_id=r.rate_card_id)
		   OR r.revision=(SELECT MAX(revision) FROM olp.rate_card_revisions
		                  WHERE rate_card_id=r.rate_card_id AND effective_at <= now()))
		  ORDER BY r.rate_card_id, r.revision DESC`, cardIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byCard := make(map[string][]CardRevision, len(cardIDs))
	for rows.Next() {
		revision, err := scanCardRevision(rows)
		if err != nil {
			return nil, err
		}
		byCard[revision.RateCardID] = append(byCard[revision.RateCardID], revision)
	}
	return byCard, rows.Err()
}

func (s *Server) createRateCard(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input rateCardInput
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
	var organizationID, projectID *string
	switch input.Owner.Kind {
	case "installation":
		if input.Owner.ID != nil {
			return access.Reply{}, access.Invalid("owner.id", "The installation card takes no owner id.")
		}
	case "organization", "project":
		if input.Owner.ID == nil {
			return access.Reply{}, access.Invalid("owner.id", "Name the "+input.Owner.Kind+".")
		}
		id, err := access.ParseUUID(*input.Owner.ID)
		if err != nil {
			return access.Reply{}, access.Invalid("owner.id", "Use a valid identifier.")
		}
		table := input.Owner.Kind + "s"
		var exists bool
		if err = tx.QueryRow(r.Context(),
			"SELECT EXISTS(SELECT 1 FROM olp."+table+" WHERE id=$1::uuid)", id).Scan(&exists); err != nil {
			return access.Reply{}, err
		}
		if !exists {
			return access.Reply{}, access.Invalid("owner.id", "No such "+input.Owner.Kind+".")
		}
		if input.Owner.Kind == "organization" {
			organizationID = &id
		} else {
			projectID = &id
		}
	default:
		return access.Reply{}, access.Invalid("owner.kind", "Use installation, organization or project.")
	}
	id := access.NewID()
	if _, err = tx.Exec(r.Context(), `INSERT INTO olp.rate_cards
        (id, organization_id, project_id, created_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid)`,
		id, organizationID, projectID, principal.UserID()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" || strings.Contains(err.Error(), "rate_cards_owner_key") {
			return access.Reply{}, access.Fail(http.StatusConflict, "rate_card_exists",
				"The owner already has a rate card.")
		}
		return access.Reply{}, fmt.Errorf("store rate card: %w", err)
	}
	card, err := scanCard(tx.QueryRow(r.Context(), cardColumns+" WHERE c.id=$1::uuid", id))
	if err != nil {
		return access.Reply{}, fmt.Errorf("store rate card: %w", err)
	}
	result := access.Reply{Status: http.StatusCreated,
		Location: "/api/v1/rate-cards/" + card.ID, Body: card}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "rate_card.create",
		"rate_card", card.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) getRateCard(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "rate_card_id")
	if err != nil {
		return access.Reply{}, err
	}
	card, err := scanCard(s.Access.Pool.QueryRow(r.Context(), cardColumns+" WHERE c.id=$1::uuid", id))
	if err != nil {
		return access.Reply{}, err
	}
	revisions, err := loadCardRevisionSummaries(r.Context(), s.Access.Pool, card.ID)
	if err != nil {
		return access.Reply{}, err
	}
	card.Revisions = revisions
	return access.OK(card), nil
}

func (s *Server) listRateCardRevisions(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "rate_card_id")
	if err != nil {
		return access.Reply{}, err
	}
	limit, err := limitParam(r.URL.Query())
	if err != nil {
		return access.Reply{}, err
	}
	before := 1 << 30
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		revision, parseErr := strconv.Atoi(raw)
		if parseErr != nil || revision < 1 {
			return access.Reply{}, access.Fail(400, "invalid_cursor", "The cursor is invalid.")
		}
		before = revision
	}
	rows, err := s.Access.Pool.Query(r.Context(), cardRevisionColumns+
		" WHERE r.rate_card_id=$1::uuid AND r.revision < $2 ORDER BY r.revision DESC LIMIT $3",
		id, before, int64(limit)+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []CardRevision{}
	for rows.Next() {
		revision, err := scanCardRevision(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, revision)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		cursor := fmt.Sprint(items[len(items)-1].Revision)
		next = &cursor
	}
	revisionIDs := make([]string, len(items))
	for i := range items {
		revisionIDs[i] = items[i].ID
	}
	pricesByRevision, err := LoadCardPricesByRevisions(r.Context(), s.Access.Pool, revisionIDs)
	if err != nil {
		return access.Reply{}, err
	}
	for i := range items {
		items[i].Prices = pricesByRevision[items[i].ID]
		if items[i].Prices == nil {
			items[i].Prices = []Price{}
		}
	}
	return access.OK(list{Items: items, NextCursor: next}), nil
}

func (s *Server) createRateCardRevision(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input cardRevisionInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	cardID, err := access.IDParam(r, "rate_card_id")
	if err != nil {
		return access.Reply{}, err
	}
	if input.Markup == nil {
		return access.Reply{}, access.Invalid("markup", "A rate card revision needs a markup.")
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
	var exists bool
	if err = tx.QueryRow(r.Context(),
		"SELECT EXISTS(SELECT 1 FROM olp.rate_cards WHERE id=$1::uuid)", cardID).Scan(&exists); err != nil {
		return access.Reply{}, err
	}
	if !exists {
		return access.Reply{}, access.Fail(http.StatusNotFound, "rate_card_not_found", "No such rate card.")
	}
	effectiveAt := time.Now().UTC()
	if input.EffectiveAt != nil {
		effectiveAt = input.EffectiveAt.UTC()
		if effectiveAt.Before(time.Now()) {
			return access.Reply{}, access.Invalid("effective_at",
				"Choose an effective time now or in the future.")
		}
	}
	var currency string
	if err = tx.QueryRow(r.Context(),
		"SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton), 'USD')").Scan(&currency); err != nil {
		return access.Reply{}, err
	}
	revision, err := CreateCardRevision(r.Context(), tx, principal.UserID(), cardID, *input.Markup, effectiveAt, input.Prices, currency, s.VendorKind)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated,
		Location: "/api/v1/rate-cards/" + cardID + "/revisions/" + fmt.Sprint(revision.Revision),
		Body:     revision}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "rate_card.publish",
		"rate_card", cardID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

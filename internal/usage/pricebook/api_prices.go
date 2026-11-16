package pricebook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/param"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func (s *API) listPricingDiscounts(r *http.Request, _ access.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(),
		discountColumns+" WHERE d.id<$1::uuid ORDER BY d.id DESC LIMIT $2",
		page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []Discount{}
	for rows.Next() {
		d, err := scanDiscount(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, d)
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(d Discount) string { return d.ID }), nil
}

func (s *API) createPricingDiscount(r *http.Request, _ access.Principal) (httpkit.Reply, error) {
	var input DiscountInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	discount, err := NormalizeDiscount(r.Context(), tx, input, s.Normalizer)
	if err != nil {
		return httpkit.Reply{}, err
	}
	discount, err = CreateDiscount(r.Context(), tx, principal.UserID(), discount)
	if err != nil {
		return httpkit.Reply{}, err
	}
	result := httpkit.Reply{Status: http.StatusCreated,
		Location: "/api/v1/pricing/discounts/" + discount.ID, Body: discount}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "pricing_discount.create",
		"pricing_discount", discount.ID, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return httpkit.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *API) listPricingRevisions(r *http.Request, p access.Principal) (httpkit.Reply, error) {
	var err error
	query := r.URL.Query()
	limit, err := param.Limit(query)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var before *int
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		revision, parseErr := strconv.Atoi(raw)
		if parseErr != nil || revision < 1 {
			return httpkit.Reply{}, httpkit.Fail(400, "invalid_cursor", "The cursor is invalid.")
		}
		before = &revision
	}
	items, next, err := ListRevisions(r.Context(), s.Access.Pool, before, limit)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(param.Page{Items: items, NextCursor: next}), nil
}

// pricingRevisionInput is the documented creation payload. effective_at is a
// pointer so an omitted field is refused instead of defaulting to the zero
// time, which would silently backdate every rate.
type pricingRevisionInput struct {
	EffectiveAt *time.Time    `json:"effective_at"`
	Prices      []rates.Price `json:"prices"`
}

func (s *API) createPricingRevision(r *http.Request, _ access.Principal) (httpkit.Reply, error) {
	var input pricingRevisionInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if input.EffectiveAt == nil {
		return httpkit.Reply{}, httpkit.Invalid("effective_at",
			"Give the time these prices take effect.")
	}
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	revision, err := CreateRevision(r.Context(), tx, principal.UserID(), *input.EffectiveAt,
		input.Prices, s.Normalizer)
	if err != nil {
		return httpkit.Reply{}, err
	}
	result := httpkit.Reply{Status: 201, Body: revision}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "pricing_revision.create",
		"pricing_revision", revision.ID, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return httpkit.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *API) referenceCatalog(r *http.Request, _ access.Principal) (httpkit.Reply, error) {
	if s.Catalog == nil {
		return httpkit.Reply{}, httpkit.Fail(503, "reference_catalog_unavailable", "This process holds no verified reference catalog.")
	}
	return httpkit.OK(s.Catalog.Summary()), nil
}

// quotePricing runs the same pricing SQL accounting runs — unpinned, at the
// simulated instant, with the given project as the charge's consumer — in a
// transaction it never commits, so quoting writes and audits nothing.
func (s *API) quotePricing(r *http.Request, _ access.Principal) (httpkit.Reply, error) {
	var input quoteInput
	if err := httpkit.DecodeUnique(r, &input, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	providerID, err := httpkit.ParseUUID(input.ProviderID)
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("provider_id", "Use a valid provider identifier.")
	}
	if input.Model == "" || len(input.Model) > 200 {
		return httpkit.Reply{}, httpkit.Invalid("model", "Name the model to quote.")
	}
	if !usage.ValidOperation(input.Operation) {
		return httpkit.Reply{}, httpkit.Invalid("operation", "Use an operation name.")
	}
	var quantities usage.AttemptUsage
	if len(input.Usage) == 0 || json.Unmarshal(input.Usage, &quantities) != nil {
		return httpkit.Reply{}, httpkit.Invalid("usage", "Use an attempt usage object.")
	}
	// A quote prices the billable shape: observed, complete usage.
	quantities.Evidence = usage.EvidenceObservedComplete
	if _, err := usage.ValidateUsage(&quantities, 0); err != nil {
		return httpkit.Reply{}, httpkit.Invalid("usage", errors.Unwrap(err).Error())
	}
	startedAt := time.Now().UTC()
	if input.StartedAt != nil {
		startedAt = input.StartedAt.UTC()
		if startedAt.After(time.Now()) {
			return httpkit.Reply{}, httpkit.Invalid("started_at", "Use a past or present time.")
		}
	}
	successful := true
	if input.Successful != nil {
		successful = *input.Successful
	}
	consumer := quoteConsumer{set: true}
	if input.ProjectID != nil {
		project, err := httpkit.ParseUUID(*input.ProjectID)
		if err != nil {
			return httpkit.Reply{}, httpkit.Invalid("project_id", "Use a valid project identifier.")
		}
		consumer.project = project
	}
	tx, err := s.Access.Pool.Begin(r.Context())
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	event := &usage.Event{Operation: input.Operation, ObservedAt: startedAt}
	pricing, err := priceUsageQuoted(r.Context(), tx, event, quantities, startedAt,
		successful, providerID, input.Model, routingPin{}, consumer)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Reply{}, httpkit.Invalid("provider_id", "Name an existing provider.")
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	if consumer.project != "" {
		var exists bool
		if err = tx.QueryRow(r.Context(),
			"SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1::uuid)", consumer.project).Scan(&exists); err != nil {
			return httpkit.Reply{}, err
		}
		if !exists {
			return httpkit.Reply{}, httpkit.Invalid("project_id", "Name an existing project.")
		}
	}
	var commitment any
	if pricing.CommitmentAmount != nil {
		commitment = map[string]any{"amount": *pricing.CommitmentAmount, "period": *pricing.CommitmentPeriod}
	}
	components := pricing.Components
	if components == nil {
		components = []string{}
	}
	reply := map[string]any{
		"currency":              pricing.Currency,
		"pricing_revision_id":   pricing.PricingRevisionID,
		"discount_id":           pricing.DiscountID,
		"rate_card_revision_id": pricing.RateCardRevisionID,
		"complete":              pricing.Complete,
		"list_cost":             pricing.ListCost,
		"cost":                  pricing.EstimatedCost,
		"charge":                pricing.Charge,
		"components":            components,
		"commitment":            commitment,
	}
	return httpkit.Reply{Status: http.StatusOK, Body: reply}, nil
}

package pricebook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/param"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func (s *API) listPricingDiscounts(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
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

func (s *API) createPricingDiscount(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input DiscountInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.Access, r, input, access.Unlocked, func(tx pgx.Tx, principal authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		discount, err := NormalizeDiscount(r.Context(), tx, input, s.Normalizer)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		discount, err = CreateDiscount(r.Context(), tx, principal.UserID(), discount)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Reply{Status: http.StatusCreated,
			Location: "/api/v1/pricing/discounts/" + discount.ID, Body: discount}
		return result, access.Event{Action: "pricing_discount.create", Resource: "pricing_discount", ID: discount.ID}, nil
	})
}

func (s *API) listPricingRevisions(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var err error
	query := r.URL.Query()
	limit, err := param.Limit(query)
	if err != nil {
		return httpkit.Reply{}, err
	}
	before, err := param.RevisionCursor(query)
	if err != nil {
		return httpkit.Reply{}, err
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

func (s *API) createPricingRevision(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input pricingRevisionInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if input.EffectiveAt == nil {
		return httpkit.Reply{}, problem.Invalid("effective_at",
			"Give the time these prices take effect.")
	}
	return access.Mutate(s.Access, r, input, access.Unlocked, func(tx pgx.Tx, principal authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		revision, err := CreateRevision(r.Context(), tx, principal.UserID(), *input.EffectiveAt,
			input.Prices, s.Normalizer)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Reply{Status: 201, Body: revision}
		return result, access.Event{Action: "pricing_revision.create", Resource: "pricing_revision", ID: revision.ID}, nil
	})
}

func (s *API) referenceCatalog(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	if s.Catalog == nil {
		return httpkit.Reply{}, problem.Fail(503, "reference_catalog_unavailable", "This process holds no verified reference catalog.")
	}
	return httpkit.OK(s.Catalog.Summary()), nil
}

// quotePricing runs the same pricing SQL accounting runs — unpinned, at the
// simulated instant, with the given project as the charge's consumer — in a
// transaction it never commits, so quoting writes and audits nothing.
func (s *API) quotePricing(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input quoteInput
	if err := httpkit.DecodeUnique(r, &input, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	providerID, err := ids.Parse(input.ProviderID)
	if err != nil {
		return httpkit.Reply{}, problem.Invalid("provider_id", "Use a valid provider identifier.")
	}
	if input.Model == "" || len(input.Model) > 200 {
		return httpkit.Reply{}, problem.Invalid("model", "Name the model to quote.")
	}
	if !usage.ValidOperation(input.Operation) {
		return httpkit.Reply{}, problem.Invalid("operation", "Use an operation name.")
	}
	var quantities usage.AttemptUsage
	if len(input.Usage) == 0 || json.Unmarshal(input.Usage, &quantities) != nil {
		return httpkit.Reply{}, problem.Invalid("usage", "Use an attempt usage object.")
	}
	// A quote prices the billable shape: observed, complete usage.
	quantities.Evidence = usage.EvidenceObservedComplete
	if _, err := usage.ValidateUsage(&quantities, 0); err != nil {
		return httpkit.Reply{}, problem.Invalid("usage", errors.Unwrap(err).Error())
	}
	startedAt := time.Now().UTC()
	if input.StartedAt != nil {
		startedAt = input.StartedAt.UTC()
		if startedAt.After(time.Now()) {
			return httpkit.Reply{}, problem.Invalid("started_at", "Use a past or present time.")
		}
	}
	successful := true
	if input.Successful != nil {
		successful = *input.Successful
	}
	consumer := quoteConsumer{set: true}
	if input.ProjectID != nil {
		project, err := ids.Parse(*input.ProjectID)
		if err != nil {
			return httpkit.Reply{}, problem.Invalid("project_id", "Use a valid project identifier.")
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
		return httpkit.Reply{}, problem.Invalid("provider_id", "Name an existing provider.")
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
			return httpkit.Reply{}, problem.Invalid("project_id", "Name an existing project.")
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

package usage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

// quoteInput is what POST /api/v1/pricing/quote prices: one attempt's usage
// against a provider's price, discount and consumer rate card, exactly as
// accounting would record it.
type quoteInput struct {
	ProviderID string          `json:"provider_id"`
	Model      string          `json:"model"`
	Operation  string          `json:"operation"`
	Usage      json.RawMessage `json:"usage"`
	StartedAt  *time.Time      `json:"started_at"`
	Successful *bool           `json:"successful"`
	ProjectID  *string         `json:"project_id"`
}

// quotePricing runs the same pricing SQL accounting runs — unpinned, at the
// simulated instant, with the given project as the charge's consumer — in a
// transaction it never commits, so quoting writes and audits nothing.
func (s *Server) quotePricing(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input quoteInput
	if err := access.DecodeUnique(r, &input, 64<<10); err != nil {
		return access.Reply{}, err
	}
	providerID, err := access.ParseUUID(input.ProviderID)
	if err != nil {
		return access.Reply{}, access.Invalid("provider_id", "Use a valid provider identifier.")
	}
	if input.Model == "" || len(input.Model) > 200 {
		return access.Reply{}, access.Invalid("model", "Name the model to quote.")
	}
	if !validOperation(input.Operation) {
		return access.Reply{}, access.Invalid("operation", "Use an operation name.")
	}
	var usage AttemptUsage
	if len(input.Usage) == 0 || json.Unmarshal(input.Usage, &usage) != nil {
		return access.Reply{}, access.Invalid("usage", "Use an attempt usage object.")
	}
	// A quote prices the billable shape: observed, complete usage.
	usage.Observed, usage.Complete, usage.BillingUncertain = true, true, false
	if _, err := validateUsage(&usage, 0); err != nil {
		return access.Reply{}, access.Invalid("usage", errors.Unwrap(err).Error())
	}
	startedAt := time.Now().UTC()
	if input.StartedAt != nil {
		startedAt = input.StartedAt.UTC()
		if startedAt.After(time.Now()) {
			return access.Reply{}, access.Invalid("started_at", "Use a past or present time.")
		}
	}
	successful := true
	if input.Successful != nil {
		successful = *input.Successful
	}
	consumer := quoteConsumer{set: true}
	if input.ProjectID != nil {
		project, err := access.ParseUUID(*input.ProjectID)
		if err != nil {
			return access.Reply{}, access.Invalid("project_id", "Use a valid project identifier.")
		}
		consumer.project = project
	}
	tx, err := s.Access.Pool.Begin(r.Context())
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	event := &Event{Operation: input.Operation, ObservedAt: startedAt}
	pricing, err := priceUsageQuoted(r.Context(), tx, event, usage, startedAt,
		successful, providerID, input.Model, routingPin{}, consumer)
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Reply{}, access.Invalid("provider_id", "Name an existing provider.")
	}
	if err != nil {
		return access.Reply{}, err
	}
	if consumer.project != "" {
		var exists bool
		if err = tx.QueryRow(r.Context(),
			"SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1::uuid)", consumer.project).Scan(&exists); err != nil {
			return access.Reply{}, err
		}
		if !exists {
			return access.Reply{}, access.Invalid("project_id", "Name an existing project.")
		}
	}
	var commitment any
	if pricing.commitmentAmount != nil {
		commitment = map[string]any{"amount": *pricing.commitmentAmount, "period": *pricing.commitmentPeriod}
	}
	components := pricing.components
	if components == nil {
		components = []string{}
	}
	reply := map[string]any{
		"currency":              pricing.currency,
		"pricing_revision_id":   pricing.pricingRevisionID,
		"discount_id":           pricing.discountID,
		"rate_card_revision_id": pricing.rateCardRevisionID,
		"complete":              pricing.complete,
		"list_cost":             pricing.listCost,
		"cost":                  pricing.estimatedCost,
		"charge":                pricing.charge,
		"components":            components,
		"commitment":            commitment,
	}
	return access.Reply{Status: http.StatusOK, Body: reply}, nil
}

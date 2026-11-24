package pricebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// AttemptPricing is one attempt resolved against the pricing revision that was
// in force when it was observed. Prices are never read at request time: a
// revision published later must not change what an old request cost.
type AttemptPricing struct {
	PricingRevisionID    *string
	Currency             *string
	Complete             bool
	EstimatedCost        *string
	Components           []string
	CommitmentWeight     *string
	CommitmentAmount     *string
	CommitmentPeriod     *string
	CommitmentScheduleID *string
	CommitmentBucket     *time.Time
	ListCost             *string
	ContractedCost       *string
	CostLines            []byte
	ChargeLines          []byte
	unguardedCostLines   []byte
	unguardedChargeLines []byte
	DiscountID           *string
	Charge               *string
	RateCardRevisionID   *string
}

type routingPin struct {
	commitmentPinned   bool
	commitmentID       *string
	pricingRevisionID  *string
	providerRevisionID *string
	pinned             bool
	vendorID           *string
	discountPinned     bool
	discountID         *string
	rateCardPinned     bool
	rateCardRevisionID *string
}

func attemptRouting(attempt *usage.Attempt) (routingPin, error) {
	var pin routingPin
	if attempt.Routing == nil {
		return pin, nil
	}
	pin.pricingRevisionID = attempt.Routing.PricingRevisionID
	if revision := attempt.Routing.ProviderRevisionID; revision != "" {
		pin.providerRevisionID = &revision
	}
	pin.pinned = pin.pricingRevisionID != nil
	if attempt.Routing.Policy == nil {
		return pin, nil
	}
	var policy struct {
		CommitmentPinned bool `json:"commitment_pinned"`
		Commitment       *struct {
			ID string `json:"id"`
		} `json:"commitment"`
		PricingPinned      *bool   `json:"pricing_pinned"`
		VendorID           *string `json:"vendor_id"`
		DiscountPinned     *bool   `json:"discount_pinned"`
		DiscountID         *string `json:"discount_id"`
		RateCardPinned     *bool   `json:"rate_card_pinned"`
		RateCardRevisionID *string `json:"rate_card_revision_id"`
	}
	if err := json.Unmarshal(*attempt.Routing.Policy, &policy); err != nil {
		return routingPin{}, fmt.Errorf("%w: attempt routing policy is malformed", usage.ErrInvalidEvent)
	}
	if policy.PricingPinned != nil && *policy.PricingPinned {
		pin.pinned = true
	}
	pin.commitmentPinned = policy.CommitmentPinned
	if policy.Commitment != nil && policy.Commitment.ID != "" {
		pin.commitmentID = &policy.Commitment.ID
	}
	pin.vendorID = policy.VendorID
	if policy.DiscountPinned != nil && *policy.DiscountPinned {
		pin.discountPinned = true
		pin.discountID = policy.DiscountID
	}
	if policy.RateCardPinned != nil && *policy.RateCardPinned {
		pin.rateCardPinned = true
		pin.rateCardRevisionID = policy.RateCardRevisionID
	}
	return pin, nil
}

var defaultVendorCases = func() string {
	var arms []string
	for _, contract := range vendors.All() {
		if contract.KindDefault && contract.ID != contract.Connector {
			arms = append(arms, fmt.Sprintf("WHEN '%s' THEN '%s'", contract.Connector, contract.ID))
		}
	}
	return strings.Join(arms, "\n                                  ")
}()

// selectPricingInputsSQL resolves the relational pricing rows (price entry,
// discount, commitment schedule, rate card revision and explicit card price)
// in force for one attempt; all cost, charge, and SKU-line math runs in Go via
// internal/usage/rates.
var selectPricingInputsSQL = `SELECT selected.pricing_revision_id::text,
        selected.currency,
        selected.price_doc,
        discount.id::text,
        discount.multiplier::text,
        commitment.id::text,
        commitment.amount::text,
        commitment.period,
        olp.commitment_bucket(commitment.period, $5::timestamptz),
        cardrev.rate_card_revision_id::text,
        cardrev.markup::text,
        explicit.doc
    FROM olp.providers provider
    LEFT JOIN olp.provider_revisions provider_revision
        ON provider_revision.id = COALESCE($7::uuid, provider.active_revision_id)
       AND provider_revision.provider_id = provider.id
    CROSS JOIN LATERAL (
        SELECT COALESCE(provider_revision.configuration->>'kind', provider.kind) AS kind,
               CASE WHEN $8::boolean THEN $9::text
                    ELSE COALESCE(CASE WHEN provider_revision.id IS NOT NULL
                                      THEN provider_revision.configuration->'options'->>'vendor_id'
                                      ELSE provider.configuration->'options'->>'vendor_id' END,
                                  CASE COALESCE(provider_revision.configuration->>'kind', provider.kind)
                                  ` + defaultVendorCases + `
                                  ELSE COALESCE(provider_revision.configuration->>'kind', provider.kind)
                              END) END AS vendor
    ) derived
    LEFT JOIN LATERAL (
        SELECT revision.id AS pricing_revision_id, to_jsonb(price) AS price_doc,
               btrim(price.currency::text) AS currency
        FROM olp.pricing_revisions revision
        JOIN olp.prices price ON price.pricing_revision_id = revision.id
        WHERE revision.effective_at <= $4
          AND ($7::uuid IS NULL OR provider_revision.id IS NOT NULL)
          AND (NOT $8::boolean OR revision.id = $6::uuid)
          AND price.provider_kind = derived.kind
          AND (price.vendor_id IS NULL OR price.vendor_id = derived.vendor)
          AND (price.provider_id IS NULL OR price.provider_id = provider.id)
          AND price.model = $2 AND price.operation = $3
        ORDER BY (price.provider_id IS NOT NULL) DESC, (price.vendor_id IS NOT NULL) DESC,
                 revision.effective_at DESC, revision.revision DESC
        LIMIT 1) selected ON true
    LEFT JOIN LATERAL (
        SELECT d.id, d.multiplier
        FROM olp.pricing_discounts d
        WHERE ($10::boolean AND d.id = $11::uuid)
           OR (NOT $10::boolean
               AND d.effective_at <= $4
               AND d.provider_kind = derived.kind
               AND (d.vendor_id IS NULL OR d.vendor_id = derived.vendor)
               AND (d.provider_id IS NULL OR d.provider_id = provider.id))
        ORDER BY (d.provider_id IS NOT NULL) DESC, (d.vendor_id IS NOT NULL) DESC,
                 d.effective_at DESC, d.created_at DESC, d.id DESC
        LIMIT 1) discount ON true
    LEFT JOIN LATERAL (
        SELECT s.id, s.amount, s.period
        FROM olp.commitment_schedules s
        WHERE s.provider_id = provider.id AND
          (($18::boolean AND s.id = $19::uuid) OR (NOT $18::boolean AND s.effective_at <= $5 AND (s.ends_at IS NULL OR s.ends_at > $5)))
        ORDER BY s.effective_at DESC LIMIT 1
    ) commitment ON true
    CROSS JOIN LATERAL (
        SELECT CASE WHEN $16::boolean
                    THEN NULLIF($17::text, '')::uuid
                    ELSE COALESCE(
                        (SELECT k.project_id FROM olp.api_keys k WHERE k.id = NULLIF($14::text, '')::uuid),
                        (SELECT p.project_id FROM olp.providers p WHERE p.id = provider.id),
                        (SELECT r.project_id FROM olp.routes r WHERE r.slug = $15)) END AS project_id
    ) consumer
    LEFT JOIN LATERAL (
        SELECT rev.id AS rate_card_revision_id, rev.markup
        FROM olp.rate_card_revisions rev
        JOIN olp.rate_cards card ON card.id = rev.rate_card_id
        WHERE ($12::boolean AND rev.id = $13::uuid)
           OR (NOT $12::boolean AND rev.effective_at <= $4
               AND (card.project_id = consumer.project_id
                    OR card.organization_id = (SELECT p.organization_id FROM olp.projects p
                                               WHERE p.id = consumer.project_id)
                    OR (card.organization_id IS NULL AND card.project_id IS NULL)))
        ORDER BY (card.project_id IS NOT NULL) DESC,
                 (card.organization_id IS NOT NULL) DESC,
                 rev.effective_at DESC, rev.revision DESC
        LIMIT 1) cardrev ON true
    LEFT JOIN LATERAL (
        SELECT to_jsonb(cp) AS doc FROM olp.rate_card_prices cp
        WHERE cp.rate_card_revision_id = cardrev.rate_card_revision_id
          AND cp.provider_kind = derived.kind
          AND (cp.vendor_id IS NULL OR cp.vendor_id = derived.vendor)
          AND cp.model = $2 AND cp.operation = $3
        ORDER BY (cp.vendor_id IS NOT NULL) DESC
        LIMIT 1) explicit ON true
    WHERE provider.id = $1::uuid`

type pricingInputs struct {
	pricingRevisionID  *string
	currency           *string
	priceDoc           []byte
	discountID         *string
	discountMultiplier *string
	commitmentID       *string
	commitmentAmount   *string
	commitmentPeriod   *string
	commitmentBucket   *time.Time
	rateCardRevisionID *string
	rateCardMarkup     *string
	explicitDoc        []byte
}

func PriceAttempt(ctx context.Context, tx pgx.Tx, event *usage.Event, attempt usage.ValidatedAttempt) (AttemptPricing, error) {
	if attempt.Attempt.Routing != nil && attempt.Attempt.Routing.Cache != nil {
		zero := "0"
		currency := attempt.Attempt.Routing.Cache.Currency
		var currencyPtr *string
		if currency != "" {
			currencyPtr = &currency
		}
		return AttemptPricing{Complete: true, EstimatedCost: &zero, Charge: &zero, ListCost: &zero, ContractedCost: &zero, Currency: currencyPtr, Components: []string{"response_cache"}}, nil
	}
	pin, err := attemptRouting(attempt.Attempt)
	if err != nil {
		return AttemptPricing{}, err
	}
	successful := attempt.Attempt.ErrorClass == nil && attempt.StatusCode != nil &&
		(*attempt.StatusCode >= 200 && *attempt.StatusCode <= 299 || event.Operation == "passthrough" && *attempt.StatusCode == 304)
	return priceUsage(ctx, tx, event, attempt.Usage, attempt.Attempt.StartedAt,
		successful, attempt.Attempt.ProviderID, attempt.Attempt.UpstreamModel, pin)
}

func PriceBaseline(ctx context.Context, tx pgx.Tx, event *usage.Event, attempt usage.ValidatedAttempt) (*string, error) {
	baseline := attempt.Attempt.Routing.Baseline
	successful := attempt.Attempt.ErrorClass == nil && attempt.StatusCode != nil &&
		*attempt.StatusCode >= 200 && *attempt.StatusCode <= 299
	pricing, err := priceUsage(ctx, tx, event, attempt.Usage, attempt.Attempt.StartedAt,
		successful, baseline.ProviderID, baseline.UpstreamModel, routingPin{vendorID: baseline.VendorID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return pricing.EstimatedCost, err
}

type quoteConsumer struct {
	set     bool
	project string
}

func priceUsage(ctx context.Context, tx pgx.Tx, event *usage.Event, u usage.AttemptUsage,
	startedAt time.Time, successful bool, providerID, model string, pin routingPin) (AttemptPricing, error) {
	return priceUsageQuoted(ctx, tx, event, u, startedAt, successful, providerID, model, pin, quoteConsumer{})
}

func priceUsageQuoted(ctx context.Context, tx pgx.Tx, event *usage.Event, u usage.AttemptUsage,
	startedAt time.Time, successful bool, providerID, model string, pin routingPin, consumer quoteConsumer) (AttemptPricing, error) {
	var in pricingInputs
	err := tx.QueryRow(ctx, selectPricingInputsSQL,
		providerID, model, event.Operation, event.ObservedAt, startedAt,
		pin.pricingRevisionID, pin.providerRevisionID, pin.pinned, pin.vendorID,
		pin.discountPinned, pin.discountID, pin.rateCardPinned, pin.rateCardRevisionID,
		event.APIKeyID, event.RouteSlug, consumer.set, consumer.project, pin.commitmentPinned, pin.commitmentID,
	).Scan(
		&in.pricingRevisionID, &in.currency, &in.priceDoc,
		&in.discountID, &in.discountMultiplier,
		&in.commitmentID, &in.commitmentAmount, &in.commitmentPeriod, &in.commitmentBucket,
		&in.rateCardRevisionID, &in.rateCardMarkup, &in.explicitDoc,
	)
	if err != nil {
		return AttemptPricing{}, fmt.Errorf("price attempt: %w", err)
	}
	return evaluatePricingInputs(in, u, startedAt, successful, !consumer.set)
}

func evaluatePricingInputs(in pricingInputs, u usage.AttemptUsage, startedAt time.Time, successful, forStorage bool) (AttemptPricing, error) {
	pricing := AttemptPricing{
		PricingRevisionID:    in.pricingRevisionID,
		Currency:             trimOptional(in.currency),
		CommitmentScheduleID: in.commitmentID,
		CommitmentBucket:     in.commitmentBucket,
		RateCardRevisionID:   in.rateCardRevisionID,
	}
	committed := in.commitmentID != nil

	var priced rates.UsagePricing
	if len(in.priceDoc) > 0 {
		price, err := rates.DecodePriceRowJSON(in.priceDoc)
		if err != nil {
			return AttemptPricing{}, fmt.Errorf("price attempt: %w", err)
		}
		priced = rates.EvaluatePrice(&price, u, startedAt, successful, in.discountMultiplier)
	}
	pricing.Complete = priced.Complete

	var costLines []byte
	if priced.Complete && u.Evidence.Complete() {
		listStr := pricingAmount(priced.ListCost, forStorage)
		contractedStr := pricingAmount(priced.Cost, forStorage)
		estimatedStr := contractedStr
		components := priced.Components
		if committed {
			estimatedStr = "0"
			components = append(slices.Clone(components), "commitment")
			pricing.CommitmentWeight = &listStr
			pricing.CommitmentAmount = in.commitmentAmount
			pricing.CommitmentPeriod = in.commitmentPeriod
		}
		if in.discountID != nil {
			pricing.DiscountID = in.discountID
		}
		pricing.ListCost = &listStr
		pricing.ContractedCost = &contractedStr
		pricing.EstimatedCost = &estimatedStr
		pricing.Components = components
		var err error
		if costLines, err = marshalPriceLines(priced.Lines); err != nil {
			return AttemptPricing{}, fmt.Errorf("price attempt: %w", err)
		}
		pricing.unguardedCostLines = costLines
		pricing.CostLines = costLinesKept(costLines, pricing.ListCost, pricing.ContractedCost)
	}

	charge, chargeLines, err := evaluateChargeLeg(in, u, startedAt, successful, committed, priced)
	if err != nil {
		return AttemptPricing{}, fmt.Errorf("price attempt: %w", err)
	}
	pricing.unguardedChargeLines = chargeLines
	if !u.Evidence.Complete() {
		charge = nil
		chargeLines = nil
	}
	if charge != nil {
		pricing.Charge = new(pricingAmount(*charge, forStorage))
	}
	pricing.ChargeLines = chargeLinesKept(chargeLines, pricing.Charge)
	return pricing, nil
}

func pricingAmount(cost rates.Cost, forStorage bool) string {
	if forStorage {
		return cost.ForStorage().String()
	}
	return cost.String()
}

func evaluateChargeLeg(in pricingInputs, u usage.AttemptUsage, startedAt time.Time, successful, committed bool, priced rates.UsagePricing) (*rates.Cost, []byte, error) {
	if len(in.explicitDoc) > 0 {
		explicitPrice, err := rates.DecodePriceRowJSON(in.explicitDoc)
		if err != nil {
			return nil, nil, err
		}
		explicit := rates.EvaluatePrice(&explicitPrice, u, startedAt, successful, nil)
		if !explicit.Complete {
			return nil, nil, nil
		}
		lines, err := marshalPriceLines(explicit.Lines)
		return &explicit.Cost, lines, err
	}
	if !priced.Complete {
		return nil, nil, nil
	}
	if committed {
		scaled, err := rates.ScalePriceLines(priced.Lines, "0")
		if err != nil {
			return nil, nil, err
		}
		lines, err := marshalPriceLines(scaled)
		return new(rates.Cost), lines, err
	}
	if in.rateCardRevisionID == nil {
		scaled, err := rates.ScalePriceLines(priced.Lines, "1")
		if err != nil {
			return nil, nil, err
		}
		lines, err := marshalPriceLines(scaled)
		return &priced.Cost, lines, err
	}
	if in.rateCardMarkup == nil {
		return nil, nil, nil
	}
	markedUp, ok := rates.ApplyMarkup(priced.Cost, *in.rateCardMarkup)
	if !ok {
		return nil, nil, nil
	}
	scaled, err := rates.ScalePriceLines(priced.Lines, *in.rateCardMarkup)
	if err != nil {
		return nil, nil, err
	}
	lines, err := marshalPriceLines(scaled)
	return &markedUp, lines, err
}

func marshalPriceLines(lines []rates.PriceLine) ([]byte, error) {
	if lines == nil {
		return nil, nil
	}
	if len(lines) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(lines)
}

func trimOptional(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

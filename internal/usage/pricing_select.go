package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/vendors"
)

// attemptPricing is one attempt resolved against the pricing revision that was
// in force when it was observed. Prices are never read at request time: a
// revision published later must not change what an old request cost.
type attemptPricing struct {
	pricingRevisionID *string
	currency          *string
	// complete says every dimension the attempt used had a rate. An attempt
	// billed on an incomplete price is recorded as unpriced instead of being
	// charged at a rate nobody configured.
	complete      bool
	estimatedCost *string
	// components names the price components the attempt was charged through,
	// which the fact stores so a report can show what produced its cost.
	components []string
	// commitmentWeight is the list cost a committed attempt divides its
	// period's commitment amount by; amount and period travel with it.
	commitmentWeight *string
	commitmentAmount *string
	commitmentPeriod *string
	// listCost is the undiscounted, uncommitted price the charge and weight
	// are measured from.
	listCost *string
	// discountID is the discount the cost was priced under; charge is what
	// the attempt's consumer was billed under the rate card revision named.
	discountID         *string
	charge             *string
	rateCardRevisionID *string
}

// routingPin is the pricing provenance the gateway recorded for one attempt.
type routingPin struct {
	pricingRevisionID  *string
	providerRevisionID *string
	pinned             bool
	vendorID           *string
	// The gateway also pins the discount and rate card revision it resolved
	// at admission; a pinned null means none applies.
	discountPinned     bool
	discountID         *string
	rateCardPinned     bool
	rateCardRevisionID *string
}

// attemptRouting reads the pin out of an attempt's routing provenance. A
// routing policy that cannot be read is an error: it decides which revision
// prices the attempt, and guessing would charge against the wrong catalogue.
func attemptRouting(attempt *Attempt) (routingPin, error) {
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
		PricingPinned      *bool   `json:"pricing_pinned"`
		VendorID           *string `json:"vendor_id"`
		DiscountPinned     *bool   `json:"discount_pinned"`
		DiscountID         *string `json:"discount_id"`
		RateCardPinned     *bool   `json:"rate_card_pinned"`
		RateCardRevisionID *string `json:"rate_card_revision_id"`
	}
	if err := json.Unmarshal(*attempt.Routing.Policy, &policy); err != nil {
		return routingPin{}, fmt.Errorf("%w: attempt routing policy is malformed", ErrInvalidEvent)
	}
	if policy.PricingPinned != nil && *policy.PricingPinned {
		pin.pinned = true
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

// defaultVendorCases are the CASE arms naming the default vendor of each
// connector kind whose vendor identifier is not the kind itself, so an attempt
// of a provider that names no vendor is priced as the vendor catalogue says.
var defaultVendorCases = func() string {
	var arms []string
	for _, contract := range vendors.All() {
		if contract.KindDefault && contract.ID != contract.Connector {
			arms = append(arms, fmt.Sprintf("WHEN '%s' THEN '%s'", contract.Connector, contract.ID))
		}
	}
	return strings.Join(arms, "\n                                  ")
}()

// priceAttemptSQL resolves the price for one attempt.
//
// The selection walks outwards from the most specific evidence: a price scoped
// to this exact provider beats a vendor-wide price, a newer effective date
// beats an older one, and a newer revision breaks ties. The vendor identity
// comes from the provider revision that actually served the attempt (a
// connector fronting a known vendor prices against that vendor), falling back
// to the catalogue's default name for the connector kind.
//
// The whole selected price row goes to olp.price_usage as jsonb, which applies
// every component the entry carries: per-million shares with their fallbacks,
// input tiers, time windows, tool fees, per-request fees and the batch
// multiplier. The usage document it prices has `complete` forced true, so its
// `complete` output answers the fact table's question — does every dimension
// the attempt used have a rate — independently of whether the attempt's usage
// is settled. The stored cost is gated on usage completeness here instead.
var priceAttemptSQL = `SELECT selected.pricing_revision_id::text,
        selected.currency,
        priced.complete AS pricing_complete,
        CASE WHEN committed.present AND priced.cost IS NOT NULL THEN '0'::numeric
             ELSE priced.cost END::text AS estimated_cost,
        CASE WHEN NOT committed.present OR priced.components IS NULL THEN priced.components
             ELSE array_append(priced.components, 'commitment') END AS components,
        CASE WHEN committed.present AND priced.cost IS NOT NULL
             THEN priced.list_cost END::text AS commitment_weight,
        CASE WHEN committed.present AND priced.cost IS NOT NULL
             THEN commitment.commitment->>'amount' END AS commitment_amount,
        CASE WHEN committed.present AND priced.cost IS NOT NULL
             THEN commitment.commitment->>'period' END AS commitment_period,
        CASE WHEN priced.cost IS NOT NULL AND discount.id IS NOT NULL
             THEN discount.id::text END AS discount_id,
        cardrev.rate_card_revision_id::text,
        charge.charge,
        priced.list_cost::text
    FROM olp.providers provider
    LEFT JOIN olp.provider_revisions provider_revision
        ON provider_revision.id = COALESCE($9::uuid, provider.active_revision_id)
       AND provider_revision.provider_id = provider.id
    CROSS JOIN LATERAL (
        SELECT COALESCE(provider_revision.configuration->>'kind', provider.kind) AS kind,
               CASE WHEN $10::boolean THEN $11::text
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
          AND ($9::uuid IS NULL OR provider_revision.id IS NOT NULL)
          AND (NOT $10::boolean OR revision.id = $8::uuid)
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
        WHERE ($12::boolean AND d.id = $13::uuid)
           OR (NOT $12::boolean
               AND d.effective_at <= $4
               AND d.provider_kind = derived.kind
               AND (d.vendor_id IS NULL OR d.vendor_id = derived.vendor)
               AND (d.provider_id IS NULL OR d.provider_id = provider.id))
        ORDER BY (d.provider_id IS NOT NULL) DESC, (d.vendor_id IS NOT NULL) DESC,
                 d.effective_at DESC, d.created_at DESC, d.id DESC
        LIMIT 1) discount ON true
    CROSS JOIN LATERAL (
        SELECT * FROM olp.price_usage(selected.price_doc, $5::jsonb, $6::timestamptz, $7::boolean,
                                      COALESCE(discount.multiplier, 1))
    ) priced
    CROSS JOIN LATERAL (
        SELECT CASE WHEN provider_revision.id IS NOT NULL
                    THEN provider_revision.configuration->'options'->'commitment'
                    ELSE provider.configuration->'options'->'commitment' END AS commitment
    ) commitment
    CROSS JOIN LATERAL (
        SELECT jsonb_typeof(commitment.commitment::jsonb) = 'object' AS present
    ) committed
    CROSS JOIN LATERAL (
        SELECT CASE WHEN $18::boolean
                    THEN NULLIF($19::text,'')::uuid
                    ELSE COALESCE(
                        (SELECT k.project_id FROM olp.api_keys k WHERE k.id = NULLIF($16::text,'')::uuid),
                        (SELECT p.project_id FROM olp.providers p WHERE p.id = provider.id),
                        (SELECT r.project_id FROM olp.routes r WHERE r.slug = $17)) END AS project_id
    ) consumer
    LEFT JOIN LATERAL (
        SELECT rev.id AS rate_card_revision_id, rev.markup
        FROM olp.rate_card_revisions rev
        JOIN olp.rate_cards card ON card.id = rev.rate_card_id
        WHERE ($14::boolean AND rev.id = $15::uuid)
           OR (NOT $14::boolean AND rev.effective_at <= $4
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
    CROSS JOIN LATERAL (
        SELECT CASE
            WHEN explicit.doc IS NOT NULL
                THEN (SELECT c.cost FROM olp.price_usage(explicit.doc, $5::jsonb,
                              $6::timestamptz, $7::boolean, 1) c)
            WHEN cardrev.rate_card_revision_id IS NULL
                THEN CASE WHEN committed.present AND priced.cost IS NOT NULL
                    THEN '0'::numeric ELSE priced.cost END
            WHEN CASE WHEN committed.present AND priced.cost IS NOT NULL
                THEN '0'::numeric ELSE priced.cost END IS NULL THEN NULL
            ELSE round(CASE WHEN committed.present AND priced.cost IS NOT NULL
                THEN '0'::numeric ELSE priced.cost END
                       * cardrev.markup, 12)
        END AS charge
    ) charge
    WHERE provider.id = $1::uuid`

// priceAttempt resolves what one attempt cost. A provider that no longer exists
// yields no row, which is an error rather than a free request: the delivery
// stays pending until the row is there or the operator removes the gap.
func priceAttempt(ctx context.Context, tx pgx.Tx, event *Event, attempt ValidatedAttempt) (attemptPricing, error) {
	pin, err := attemptRouting(attempt.Attempt)
	if err != nil {
		return attemptPricing{}, err
	}
	successful := attempt.Attempt.ErrorClass == nil && attempt.StatusCode != nil &&
		*attempt.StatusCode >= 200 && *attempt.StatusCode <= 299
	return priceUsage(ctx, tx, event, attempt.Usage, attempt.Attempt.StartedAt,
		successful, attempt.Attempt.ProviderID, attempt.Attempt.UpstreamModel, pin)
}

// priceBaseline prices an attempt's usage on the baseline a selector avoided,
// at the price in effect when the attempt ran. It invents no usage: a
// baseline that cannot price the attempt's dimensions has no cost.
func priceBaseline(ctx context.Context, tx pgx.Tx, event *Event, attempt ValidatedAttempt) (*string, error) {
	baseline := attempt.Attempt.Routing.Baseline
	successful := attempt.Attempt.ErrorClass == nil && attempt.StatusCode != nil &&
		*attempt.StatusCode >= 200 && *attempt.StatusCode <= 299
	pricing, err := priceUsage(ctx, tx, event, attempt.Usage, attempt.Attempt.StartedAt,
		successful, baseline.ProviderID, baseline.UpstreamModel, routingPin{vendorID: baseline.VendorID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return pricing.estimatedCost, err
}

// quoteConsumer names an explicit consumer project for the charge leg, used
// only by the pricing quote: accounting always derives the consumer from the
// attempt's key, provider and route.
type quoteConsumer struct {
	set     bool
	project string
}

func priceUsage(ctx context.Context, tx pgx.Tx, event *Event, usage AttemptUsage,
	startedAt time.Time, successful bool, providerID, model string, pin routingPin) (attemptPricing, error) {
	return priceUsageQuoted(ctx, tx, event, usage, startedAt, successful, providerID, model, pin, quoteConsumer{})
}

func priceUsageQuoted(ctx context.Context, tx pgx.Tx, event *Event, usage AttemptUsage,
	startedAt time.Time, successful bool, providerID, model string, pin routingPin, consumer quoteConsumer) (attemptPricing, error) {
	// The pricing function returns a cost only for complete usage. The fact
	// still needs to know whether an incomplete attempt's dimensions had
	// rates, so the usage document it prices reports completeness: its
	// `complete` output is then "every used dimension has a rate", which is
	// the flag unpriced facts are recorded under, and the cost it returns is
	// gated on the usage being complete below.
	documented := usage
	documented.Complete = true
	document, err := json.Marshal(documented)
	if err != nil {
		return attemptPricing{}, fmt.Errorf("price attempt: %w", err)
	}
	var pricing attemptPricing
	var cost *string
	err = tx.QueryRow(ctx, priceAttemptSQL,
		providerID, model, event.Operation, event.ObservedAt,
		string(document), startedAt, successful,
		pin.pricingRevisionID, pin.providerRevisionID, pin.pinned, pin.vendorID,
		pin.discountPinned, pin.discountID, pin.rateCardPinned, pin.rateCardRevisionID,
		event.APIKeyID, event.RouteSlug, consumer.set, consumer.project,
	).Scan(&pricing.pricingRevisionID, &pricing.currency, &pricing.complete, &cost,
		&pricing.components, &pricing.commitmentWeight, &pricing.commitmentAmount, &pricing.commitmentPeriod,
		&pricing.discountID, &pricing.rateCardRevisionID, &pricing.charge, &pricing.listCost)
	if err != nil {
		return attemptPricing{}, fmt.Errorf("price attempt: %w", err)
	}
	if usage.Complete && pricing.complete {
		pricing.estimatedCost = cost
	} else {
		pricing.components = nil
		pricing.commitmentWeight, pricing.commitmentAmount, pricing.commitmentPeriod = nil, nil, nil
		pricing.discountID = nil
	}
	if !usage.Complete {
		// A charge, like a cost, is recorded only when the usage it bills is
		// complete; an explicit rate that cannot price returns NULL either way.
		pricing.charge = nil
	}
	if pricing.currency != nil {
		currency := strings.TrimSpace(*pricing.currency)
		pricing.currency = &currency
	}
	return pricing, nil
}

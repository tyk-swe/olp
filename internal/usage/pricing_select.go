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
}

// routingPin is the pricing provenance the gateway recorded for one attempt.
type routingPin struct {
	pricingRevisionID  *string
	providerRevisionID *string
	pinned             bool
	vendorID           *string
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
	revision := attempt.Routing.ProviderRevisionID
	pin.providerRevisionID = &revision
	pin.pinned = pin.pricingRevisionID != nil
	if attempt.Routing.Policy == nil {
		return pin, nil
	}
	var policy struct {
		PricingPinned *bool   `json:"pricing_pinned"`
		VendorID      *string `json:"vendor_id"`
	}
	if err := json.Unmarshal(*attempt.Routing.Policy, &policy); err != nil {
		return routingPin{}, fmt.Errorf("%w: attempt routing policy is malformed", ErrInvalidEvent)
	}
	if policy.PricingPinned != nil && *policy.PricingPinned {
		pin.pinned = true
	}
	pin.vendorID = policy.VendorID
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
             THEN commitment.commitment->>'period' END AS commitment_period
    FROM olp.providers provider
    LEFT JOIN olp.provider_revisions provider_revision
        ON provider_revision.id = COALESCE($9::uuid, provider.active_revision_id)
       AND provider_revision.provider_id = provider.id
    LEFT JOIN LATERAL (
        SELECT revision.id AS pricing_revision_id, to_jsonb(price) AS price_doc,
               btrim(price.currency::text) AS currency
        FROM olp.pricing_revisions revision
        JOIN olp.prices price ON price.pricing_revision_id = revision.id
        WHERE revision.effective_at <= $4
          AND ($9::uuid IS NULL OR provider_revision.id IS NOT NULL)
          AND (NOT $10::boolean OR revision.id = $8::uuid)
          AND price.provider_kind = COALESCE(provider_revision.configuration->>'kind', provider.kind)
          AND (price.vendor_id IS NULL OR price.vendor_id = CASE WHEN $10::boolean THEN $11::text
                ELSE COALESCE(CASE WHEN provider_revision.id IS NOT NULL
                                  THEN provider_revision.configuration->'options'->>'vendor_id'
                                  ELSE provider.configuration->'options'->>'vendor_id' END,
                              CASE COALESCE(provider_revision.configuration->>'kind', provider.kind)
                                  ` + defaultVendorCases + `
                                  ELSE COALESCE(provider_revision.configuration->>'kind', provider.kind)
                              END) END)
          AND (price.provider_id IS NULL OR price.provider_id = provider.id)
          AND price.model = $2 AND price.operation = $3
        ORDER BY (price.provider_id IS NOT NULL) DESC, (price.vendor_id IS NOT NULL) DESC,
                 revision.effective_at DESC, revision.revision DESC
        LIMIT 1) selected ON true
    CROSS JOIN LATERAL (
        SELECT * FROM olp.price_usage(selected.price_doc, $5::jsonb, $6::timestamptz, $7::boolean)
    ) priced
    CROSS JOIN LATERAL (
        SELECT CASE WHEN provider_revision.id IS NOT NULL
                    THEN provider_revision.configuration->'options'->'commitment'
                    ELSE provider.configuration->'options'->'commitment' END AS commitment
    ) commitment
    CROSS JOIN LATERAL (
        SELECT jsonb_typeof(commitment.commitment::jsonb) = 'object' AS present
    ) committed
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

func priceUsage(ctx context.Context, tx pgx.Tx, event *Event, usage AttemptUsage,
	startedAt time.Time, successful bool, providerID, model string, pin routingPin) (attemptPricing, error) {
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
	).Scan(&pricing.pricingRevisionID, &pricing.currency, &pricing.complete, &cost,
		&pricing.components, &pricing.commitmentWeight, &pricing.commitmentAmount, &pricing.commitmentPeriod)
	if err != nil {
		return attemptPricing{}, fmt.Errorf("price attempt: %w", err)
	}
	if usage.Complete && pricing.complete {
		pricing.estimatedCost = cost
	} else {
		pricing.components = nil
		pricing.commitmentWeight, pricing.commitmentAmount, pricing.commitmentPeriod = nil, nil, nil
	}
	if pricing.currency != nil {
		currency := strings.TrimSpace(*pricing.currency)
		pricing.currency = &currency
	}
	return pricing, nil
}

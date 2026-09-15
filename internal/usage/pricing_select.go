package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
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

// priceAttemptSQL resolves the price for one attempt.
//
// The selection walks outwards from the most specific evidence: a price scoped
// to this exact provider beats a vendor-wide price, a newer effective date
// beats an older one, and a newer revision breaks ties. The vendor identity
// comes from the provider revision that actually served the attempt (a
// connector fronting a known vendor prices against that vendor), falling back
// to the catalogue's default name for the connector kind.
//
// `$5` (input tokens) is cache inclusive, so the uncached portion is `$5 - $9`.
// When the revision carries no cached tier the whole input count keeps billing
// at the full input rate, exactly as it did before the tier existed, so history
// stays comparable. The split is plain arithmetic rather than GREATEST/LEAST
// around the subtraction because those skip NULL arguments: an attempt that
// reports cached tokens without the total they came out of must propagate the
// missing total and be recorded as unpriced, not charged for its cached
// portion at a rate nothing checked. The same reason keeps the completeness
// test and the charge behind one shared expression, so neither can admit a
// dimension the other refuses to price.
const priceAttemptSQL = `SELECT selected.pricing_revision_id::text,
        selected.currency,
        priced.complete AS pricing_complete,
        CASE WHEN $8::boolean AND priced.complete
             THEN (COALESCE(priced.input_charge / 1000000, 0)
                 + COALESCE($6::numeric * selected.output_per_million / 1000000, 0)
                 + COALESCE($7::numeric * selected.unit_price, 0))::text
             ELSE NULL END AS estimated_cost
    FROM olp_go.providers provider
    LEFT JOIN olp_go.provider_revisions provider_revision
        ON provider_revision.id = COALESCE($11::uuid, provider.active_revision_id)
       AND provider_revision.provider_id = provider.id
    LEFT JOIN LATERAL (
        SELECT revision.id AS pricing_revision_id, price.input_per_million,
               price.cached_input_per_million, price.output_per_million, price.unit_price,
               btrim(price.currency::text) AS currency
        FROM olp_go.pricing_revisions revision
        JOIN olp_go.prices price ON price.pricing_revision_id = revision.id
        WHERE revision.effective_at <= $4
          AND ($11::uuid IS NULL OR provider_revision.id IS NOT NULL)
          AND (NOT $12::boolean OR revision.id = $10::uuid)
          AND price.provider_kind = COALESCE(provider_revision.configuration->>'kind', provider.kind)
          AND (price.vendor_id IS NULL OR price.vendor_id = CASE WHEN $12::boolean THEN $13::text
                ELSE COALESCE(CASE WHEN provider_revision.id IS NOT NULL
                                  THEN provider_revision.configuration->'options'->>'vendor_id'
                                  ELSE provider.configuration->'options'->>'vendor_id' END,
                              CASE COALESCE(provider_revision.configuration->>'kind', provider.kind)
                                  WHEN 'gemini' THEN 'google'
                                  WHEN 'vertex_ai' THEN 'google-vertex'
                                  WHEN 'bedrock' THEN 'amazon-bedrock'
                                  WHEN 'azure_openai' THEN 'azure'
                                  ELSE COALESCE(provider_revision.configuration->>'kind', provider.kind)
                              END) END)
          AND (price.provider_id IS NULL OR price.provider_id = provider.id)
          AND price.model = $2 AND price.operation = $3
        ORDER BY (price.provider_id IS NOT NULL) DESC, (price.vendor_id IS NOT NULL) DESC,
                 revision.effective_at DESC, revision.revision DESC
        LIMIT 1) selected ON true
    CROSS JOIN LATERAL (
        SELECT selected.pricing_revision_id IS NOT NULL
                   AND (($5::bigint IS NULL AND $9::bigint IS NULL)
                        OR ($5::bigint IS NOT NULL AND selected.input_per_million IS NOT NULL))
                   AND ($6::bigint IS NULL OR selected.output_per_million IS NOT NULL)
                   AND ($7::numeric IS NULL OR selected.unit_price IS NOT NULL) AS complete,
               CASE WHEN selected.cached_input_per_million IS NULL
                    THEN $5::numeric * selected.input_per_million
                    ELSE ($5::numeric - LEAST(COALESCE($9::bigint, 0)::numeric, $5::numeric))
                             * selected.input_per_million
                         + LEAST(COALESCE($9::bigint, 0)::numeric, $5::numeric)
                             * selected.cached_input_per_million
               END AS input_charge) priced
    WHERE provider.id = $1::uuid`

// priceAttempt resolves what one attempt cost. A provider that no longer exists
// yields no row, which is an error rather than a free request: the delivery
// stays pending until the row is there or the operator removes the gap.
func priceAttempt(ctx context.Context, tx pgx.Tx, event *Event, attempt ValidatedAttempt) (attemptPricing, error) {
	pin, err := attemptRouting(attempt.Attempt)
	if err != nil {
		return attemptPricing{}, err
	}
	var pricing attemptPricing
	err = tx.QueryRow(ctx, priceAttemptSQL,
		attempt.Attempt.ProviderID, attempt.Attempt.UpstreamModel, event.Operation, event.ObservedAt,
		attempt.Usage.InputTokens, attempt.Usage.OutputTokens, attempt.Usage.MediaUnits,
		attempt.Usage.Complete, attempt.Usage.CachedInputTokens,
		pin.pricingRevisionID, pin.providerRevisionID, pin.pinned, pin.vendorID,
	).Scan(&pricing.pricingRevisionID, &pricing.currency, &pricing.complete, &pricing.estimatedCost)
	if err != nil {
		return attemptPricing{}, fmt.Errorf("price attempt: %w", err)
	}
	if pricing.currency != nil {
		currency := strings.TrimSpace(*pricing.currency)
		pricing.currency = &currency
	}
	return pricing, nil
}

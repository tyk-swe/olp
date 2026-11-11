package rates

import (
	"fmt"
	"math/big"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

// Cost prices one attempt's usage against this price under the pinned
// discount (nil discounts nothing). The rates are parsed once when the
// routing inputs are loaded, so pricing a request does not parse them again.
func (p *RoutingPrice) Cost(u usage.AttemptUsage, startedAt time.Time, successful bool, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	var multiplier *big.Int
	if discount != nil {
		multiplier = discount.multiplierUnits
	}
	return p.rates.cost(u, startedAt, successful, multiplier)
}

// CostBound is the most a request of input tokens and, for a generation, output
// tokens can cost under this price: a ceiling for admission to reserve, never an
// estimate of what the request will cost. Input is charged at the highest of
// the input-side rates the price has, because nothing tells admission whether
// the provider will read or write its cache or share the input across
// modalities, and each division by a million is rounded up. The bound includes
// a per-request fee and the window multiplier in force at `at`; tool fees and
// batch terms are excluded. It is false when the price lacks a rate the request
// needs, which is a request nobody can price.
func (p *RoutingPrice) CostBound(input, output int64, generation bool, at time.Time, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	return p.costBound(input, output, generation, discount, p.rates.windowAt(at))
}

func (p *RoutingPrice) MaximumWindowCostBound(input, output int64, generation bool, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	return p.costBound(input, output, generation, discount, p.rates.maximumWindow())
}

func (p *RoutingPrice) costBound(input, output int64, generation bool, discount *RoutingDiscount, window *big.Int) (Cost, bool) {
	r := p.rates
	if !r.valid {
		return Cost{}, false
	}
	rates := r.selectRates(&input)
	if !rates.inputHead() || generation && !rates.outputHead() {
		return Cost{}, false
	}
	rate := maxRate(rates.input, rates.cached, rates.cacheWrite, rates.cacheWrite5m,
		rates.cacheWrite1h, rates.audioInput, rates.cachedAudioInput,
		rates.imageInput, rates.cachedImageInput)
	bound := ceilPerMillion(max(input, 0), rate)
	if generation {
		bound.Add(bound, ceilPerMillion(max(output, 0),
			maxRate(rates.output, rates.audioOutput, rates.imageOutput)))
	}
	if r.perRequest != nil {
		bound.Add(bound, r.perRequest)
	}
	if window != nil {
		// The product carries twelve places of the multiplier's scale; round
		// the bound up to a whole unit so it never under-covers the request.
		bound.Mul(bound, window)
		bound.Add(bound, new(big.Int).Sub(costUnit, big.NewInt(1)))
		bound.Quo(bound, costUnit)
	}
	if discount != nil {
		bound.Mul(bound, discount.multiplierUnits)
		bound.Add(bound, new(big.Int).Sub(costUnit, big.NewInt(1)))
		bound.Quo(bound, costUnit)
	}
	return clampCost(bound), true
}

// CostFloor is the least one attempt could cost on this price at `at` under the
// pinned discount: the input estimate at the lowest input-side rate the
// selected rate set carries (the estimate itself picks the tier), no output,
// plus the per-request fee, times the window multiplier and the discount,
// rounded down. It is false when no input rate exists.
func (p *RoutingPrice) CostFloor(input int64, at time.Time, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	r := p.rates
	if !r.valid {
		return Cost{}, false
	}
	rates := r.selectRates(&input)
	if !rates.inputHead() {
		return Cost{}, false
	}
	rate := minRate(rates.input, rates.cached, rates.cacheWrite, rates.cacheWrite5m,
		rates.cacheWrite1h, rates.audioInput, rates.cachedAudioInput,
		rates.imageInput, rates.cachedImageInput)
	floor := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(max(input, 0)), rate), perMillion)
	if r.perRequest != nil {
		floor.Add(floor, r.perRequest)
	}
	if window := r.windowAt(at); window != nil {
		floor.Mul(floor, window)
		floor.Quo(floor, costUnit)
	}
	if discount != nil {
		floor.Mul(floor, discount.multiplierUnits)
		floor.Quo(floor, costUnit)
	}
	return clampCost(floor), true
}

// Charge resolves what one billable attempt's consumer pays: the card's
// explicit rate when one matches the attempt's dimensions, else the attempt's
// already-rounded cost times the card's markup, else the cost itself when no
// card applies. Discounts and commitments never apply to explicit rates, so a
// card's explicit price costs the usage itself. False is no charge — the
// cost it multiplies, or the usage an explicit rate cannot price.
func (r *RoutingCardRevision) Charge(u usage.AttemptUsage, startedAt time.Time, successful bool,
	cost *Cost, kind, vendor, model, operation string) (Cost, bool) {
	if explicit := r.Explicit(kind, vendor, model, operation); explicit != nil {
		return explicit.rates.cost(u, startedAt, successful, nil)
	}
	if cost == nil {
		return Cost{}, false
	}
	if cost.units == nil {
		// A zero cost (a committed attempt) charges zero under any markup.
		return Cost{units: new(big.Int)}, true
	}
	// Round the already-rounded cost by the markup in one half-up rounding.
	total := new(big.Int).Mul(cost.units, r.markupUnits)
	total.Add(total, new(big.Int).Quo(costUnit, big.NewInt(2)))
	total.Quo(total, costUnit)
	return clampCost(total), true
}

// ChargeBound is the reservation-equivalent of Charge: the explicit rate's
// own bound, else the cost bound times markup rounded up. False when nothing
// can bound what the consumer pays.
func (r *RoutingCardRevision) ChargeBound(input, output int64, generation bool, at time.Time,
	bound Cost, kind, vendor, model, operation string) (Cost, bool) {
	if explicit := r.Explicit(kind, vendor, model, operation); explicit != nil {
		return explicit.CostBound(input, output, generation, at, nil)
	}
	if bound.units == nil {
		// A zero bound charges zero under any markup.
		return Cost{units: new(big.Int)}, true
	}
	total := new(big.Int).Mul(bound.units, r.markupUnits)
	total.Add(total, new(big.Int).Sub(costUnit, big.NewInt(1)))
	total.Quo(total, costUnit)
	return clampCost(total), true
}

func (p *RoutingPrice) WorkflowModelCostBound(input, output int64, discount *RoutingDiscount) (Cost, bool) {
	if p == nil || input < 1 || output < 1 {
		return Cost{}, false
	}
	for _, fee := range p.ToolCalls {
		cost, ok := ParseCost(fee)
		if !ok || !cost.IsZero() {
			return Cost{}, false
		}
	}
	return workflowModelCostThroughTiers(p, discount, input, output, p.Tiers)
}

type RoutingPrice struct {
	Price
	RevisionID    string    `json:"revision_id"`
	Revision      int       `json:"revision"`
	EffectiveAt   time.Time `json:"effective_at"`
	ScopePriority int       `json:"scope_priority"`

	rates *priceRates
}

// NewRoutingPrice derives what pricing a request needs from a loaded price, so
// a request never parses rates again. Every copy shares the parsed rates.
func NewRoutingPrice(loaded RoutingPrice) *RoutingPrice {
	loaded.rates = parseRates(&loaded.Price)
	loaded.ScopePriority = scopePriority(loaded.ProviderID, loaded.VendorID)
	return &loaded
}

func scopePriority(providerID, vendorID *string) int {
	switch {
	case providerID != nil && vendorID != nil:
		return 3
	case providerID != nil:
		return 2
	case vendorID != nil:
		return 1
	default:
		return 0
	}
}

// RoutingDiscount is one multiplier the plan may pin on an attempt.
type RoutingDiscount struct {
	ID           string    `json:"id"`
	ProviderKind string    `json:"provider_kind"`
	VendorID     *string   `json:"vendor_id"`
	ProviderID   *string   `json:"provider_id"`
	Multiplier   string    `json:"multiplier"`
	EffectiveAt  time.Time `json:"effective_at"`
	CreatedAt    time.Time `json:"created_at"`

	multiplierUnits *big.Int
}

func NewRoutingDiscount(loaded RoutingDiscount) (*RoutingDiscount, error) {
	units, ok := scaledDecimal(loaded.Multiplier, costDigits)
	if !ok {
		return nil, fmt.Errorf("stored discount multiplier %q is invalid", loaded.Multiplier)
	}
	loaded.multiplierUnits = units
	return &loaded, nil
}

// RoutingCardRevision is one card revision: a markup and explicit prices.
type RoutingCardRevision struct {
	ID          string         `json:"id"`
	Revision    int            `json:"revision"`
	Markup      string         `json:"markup"`
	EffectiveAt time.Time      `json:"effective_at"`
	Prices      []RoutingPrice `json:"prices"`

	markupUnits *big.Int
}

func NewRoutingCardRevision(loaded RoutingCardRevision) (*RoutingCardRevision, error) {
	units, ok := scaledDecimal(loaded.Markup, costDigits)
	if !ok {
		return nil, fmt.Errorf("stored rate card markup %q is invalid", loaded.Markup)
	}
	loaded.markupUnits = units
	return &loaded, nil
}

func (p *RoutingPrice) Scalar(operation string) *big.Rat {
	if p == nil {
		return nil
	}
	amounts := []*string{p.UnitPrice}
	if operation == "generation" || operation == "decision" {
		amounts = []*string{p.InputPerMillion, p.OutputPerMillion}
	}
	if operation == "embeddings" {
		amounts = []*string{p.InputPerMillion}
	}
	if operation == "search" {
		amounts = []*string{p.PerRequest}
	}
	total := new(big.Rat)
	for _, amount := range amounts {
		if amount == nil {
			return nil
		}
		v, ok := new(big.Rat).SetString(*amount)
		if !ok {
			return nil
		}
		total.Add(total, v)
	}
	return total
}

// Explicit is the card revision's rate for one attempt's dimensions, the
// vendor-scoped entry winning over the unscoped one.
func (r *RoutingCardRevision) Explicit(kind, vendor, model, operation string) *RoutingPrice {
	if r == nil {
		return nil
	}
	var found *RoutingPrice
	for index := range r.Prices {
		p := &r.Prices[index]
		if p.ProviderKind != kind || p.Model != model || p.Operation != operation ||
			p.VendorID != nil && *p.VendorID != vendor {
			continue
		}
		if found == nil || p.VendorID != nil {
			found = p
		}
	}
	return found
}

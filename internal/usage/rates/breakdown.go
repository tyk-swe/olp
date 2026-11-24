package rates

import (
	"cmp"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/usage"
)

type PriceLine struct {
	Component           string  `json:"component"`
	Tier                *string `json:"tier"`
	Unit                string  `json:"unit"`
	Quantity            string  `json:"quantity"`
	ListUnitPrice       string  `json:"list_unit_price"`
	ContractedUnitPrice string  `json:"contracted_unit_price"`
}

type UsagePricing struct {
	Complete   bool
	ListCost   Cost
	Cost       Cost
	Components []string
	Lines      []PriceLine
}

type rawPriceLine struct {
	component string
	tier      *string
	unit      string
	quantity  decimal.Decimal
	baseRate  decimal.Decimal
}

type detailAccumulator struct {
	components []string
	lines      []rawPriceLine
}

func (a *detailAccumulator) addComponent(name string) {
	a.components = append(a.components, name)
}

func (a *detailAccumulator) addLine(component string, tier *string, unit string, quantity, baseRate decimal.Decimal) {
	a.lines = append(a.lines, rawPriceLine{
		component: component,
		tier:      tier,
		unit:      unit,
		quantity:  quantity,
		baseRate:  baseRate,
	})
}

// EvaluatePrice prices one attempt's dimensions against price without gating on
// usage.Evidence.Complete(), returning list cost, discounted cost, sorted
// component names, and merged SKU price lines from the single Go engine.
func EvaluatePrice(price *Price, u usage.AttemptUsage, startedAt time.Time, successful bool, discount *string) UsagePricing {
	if price == nil {
		return UsagePricing{}
	}
	var discountUnits *big.Int
	if discount != nil {
		units, ok := scaledDecimal(*discount, costDigits)
		if !ok {
			return UsagePricing{}
		}
		discountUnits = units
	}
	return parseRates(price).evaluate(u, startedAt, successful, discountUnits, true)
}

func (r *priceRates) evaluate(u usage.AttemptUsage, startedAt time.Time, successful bool, discount *big.Int, details bool) UsagePricing {
	if !r.valid {
		return UsagePricing{}
	}
	if u.Deferred && u.InvocationCount != nil && *u.InvocationCount > 1 && len(r.tiers) > 0 {
		return UsagePricing{}
	}
	var acc *detailAccumulator
	if details {
		acc = &detailAccumulator{}
	}
	selected, matchedTier := r.selectTier(u.InputTokens)
	var tierLabel *string
	if matchedTier != nil {
		label := strconv.FormatInt(*matchedTier, 10)
		tierLabel = &label
		if acc != nil {
			acc.addComponent("tiers")
		}
	}
	total, ok := r.accumulateBase(selected, tierLabel, u, successful, acc)
	if !ok {
		return UsagePricing{}
	}
	window := r.windowAt(startedAt)
	if window != nil && acc != nil {
		acc.addComponent("windows")
	}
	var batch *big.Int
	if u.Batch {
		if r.batch == nil {
			return UsagePricing{}
		}
		batch = r.batch
		if acc != nil {
			acc.addComponent("batch_multiplier")
		}
	}
	if discount != nil && discount.Cmp(costUnit) != 0 && acc != nil {
		acc.addComponent("discount")
	}
	return r.finalizeEvaluation(total, u, window, batch, discount, acc)
}

func (r *priceRates) accumulateBase(selected *rateSet, tierLabel *string, u usage.AttemptUsage, successful bool, acc *detailAccumulator) (*big.Int, bool) {
	total := new(big.Int)
	var collect tokenCollector
	if acc != nil {
		collect = func(field string, tokens int64, rate *big.Int) {
			acc.addComponent(field)
			acc.addLine(field, tierLabel, "1000000 Tokens", decimal.New(tokens, -6), decimalFromScaled(rate, costDigits))
		}
	}
	if u.InputTokens == nil {
		if !inputSharesAbsent(u) {
			return nil, false
		}
	} else {
		if !selected.inputHead() {
			return nil, false
		}
		charge, ok := selected.inputCharge(u, collect)
		if !ok {
			return nil, false
		}
		total.Add(total, quotient(charge))
	}
	if u.OutputTokens == nil {
		if u.AudioOutputTokens != nil || u.ImageOutputTokens != nil {
			return nil, false
		}
	} else {
		if !selected.outputHead() {
			return nil, false
		}
		charge, ok := selected.outputCharge(u, collect)
		if !ok {
			return nil, false
		}
		total.Add(total, quotient(charge))
	}
	if u.MediaUnits != nil {
		if r.unit == nil {
			return nil, false
		}
		units, ok := scaledDecimal(*u.MediaUnits, mediaDigits)
		if !ok {
			return nil, false
		}
		if units.Sign() > 0 && acc != nil {
			acc.addComponent("unit_price")
			acc.addLine("unit_price", nil, "Units", decimalFromScaled(units, mediaDigits), decimalFromScaled(r.unit, costDigits))
		}
		total.Add(total, new(big.Int).Mul(units, r.unit))
	}
	invocations := int64(1)
	if u.Deferred {
		invocations = 0
	}
	if u.InvocationCount != nil {
		invocations = *u.InvocationCount
	}
	if invocations < 0 {
		return nil, false
	}
	if successful && invocations > 0 && r.perRequest != nil {
		total.Add(total, new(big.Int).Mul(new(big.Int).Mul(r.perRequest, perMillion), big.NewInt(invocations)))
		if acc != nil {
			acc.addComponent("per_request")
			acc.addLine("per_request", nil, "Requests", decimal.NewFromInt(invocations), decimalFromScaled(r.perRequest, costDigits))
		}
	}
	for tool, calls := range u.ToolCalls {
		if calls <= 0 {
			continue
		}
		fee, priced := r.tools[tool]
		if !priced {
			return nil, false
		}
		total.Add(total, new(big.Int).Mul(new(big.Int).Mul(big.NewInt(calls), fee), perMillion))
		if acc != nil {
			acc.addComponent("tool_calls." + tool)
			acc.addLine("tool_calls."+tool, nil, "Calls", decimal.NewFromInt(calls), decimalFromScaled(fee, costDigits))
		}
	}
	return total, true
}

func (r *priceRates) finalizeEvaluation(total *big.Int, u usage.AttemptUsage, window, batch, discount *big.Int, acc *detailAccumulator) UsagePricing {
	var listCost, cost Cost
	if usage.RetainedValues(u) {
		ratTotal, ok := r.retainedTotal(total, u, acc)
		if !ok {
			return UsagePricing{}
		}
		cost = roundRetainedCost(ratTotal, window, batch, discount)
		if acc != nil {
			listCost = roundRetainedCost(ratTotal, window, batch, nil)
		}
	} else {
		cost = roundScaledTotal(total, window, batch, discount)
		if acc != nil {
			listCost = roundScaledTotal(total, window, batch, nil)
		}
	}
	result := UsagePricing{
		Complete: true,
		ListCost: listCost,
		Cost:     cost,
	}
	if acc != nil {
		result.Components = sortedUniqueStrings(acc.components)
		result.Lines = buildPriceLines(acc.lines, window, batch, discount)
	}
	return result
}

func roundScaledTotal(base, window, batch, discount *big.Int) Cost {
	total := new(big.Int).Set(base)
	if window == nil && batch == nil && discount == nil {
		total.Add(total, big.NewInt(500_000))
		total.Quo(total, perMillion)
		return Cost{units: total}
	}
	divisorDigits := 6
	if window != nil {
		total.Mul(total, window)
		divisorDigits += costDigits
	}
	if batch != nil {
		total.Mul(total, batch)
		divisorDigits += costDigits
	}
	if discount != nil {
		total.Mul(total, discount)
		divisorDigits += costDigits
	}
	divisor := new(big.Int).Exp(bigTen, big.NewInt(int64(divisorDigits)), nil)
	total.Add(total, new(big.Int).Quo(divisor, big.NewInt(2)))
	total.Quo(total, divisor)
	return Cost{units: total}
}

func ApplyMarkup(cost Cost, markup string) (Cost, bool) {
	units, ok := scaledDecimal(markup, costDigits)
	if !ok {
		return Cost{}, false
	}
	if cost.IsZero() {
		return Cost{units: new(big.Int)}, true
	}
	return Cost{units: applyMarkupUnits(cost, units)}, true
}

func applyMarkupUnits(cost Cost, markupUnits *big.Int) *big.Int {
	total := new(big.Int).Mul(cost.units, markupUnits)
	total.Add(total, new(big.Int).Quo(costUnit, big.NewInt(2)))
	return total.Quo(total, costUnit)
}

func ScalePriceLines(lines []PriceLine, factor string) ([]PriceLine, error) {
	if lines == nil {
		return nil, nil
	}
	multiplier, err := decimal.NewFromString(factor)
	if err != nil {
		return nil, err
	}
	scaled := make([]mergedPriceLineInput, 0, len(lines))
	for _, line := range lines {
		qty, err := decimal.NewFromString(line.Quantity)
		if err != nil {
			return nil, err
		}
		unitPrice, err := decimal.NewFromString(line.ContractedUnitPrice)
		if err != nil {
			return nil, err
		}
		price := unitPrice.Mul(multiplier)
		scaled = append(scaled, mergedPriceLineInput{
			component:       line.Component,
			tier:            line.Tier,
			unit:            line.Unit,
			quantity:        qty,
			listPrice:       price,
			contractedPrice: price,
		})
	}
	return mergeAndSortPriceLines(scaled), nil
}

type mergedPriceLineInput struct {
	component       string
	tier            *string
	unit            string
	quantity        decimal.Decimal
	listPrice       decimal.Decimal
	contractedPrice decimal.Decimal
}

func buildPriceLines(raw []rawPriceLine, window, batch, discount *big.Int) []PriceLine {
	mult := decimal.NewFromInt(1)
	if window != nil {
		mult = mult.Mul(decimalFromScaled(window, costDigits))
	}
	if batch != nil {
		mult = mult.Mul(decimalFromScaled(batch, costDigits))
	}
	disc := decimal.NewFromInt(1)
	if discount != nil {
		disc = decimalFromScaled(discount, costDigits)
	}
	inputs := make([]mergedPriceLineInput, 0, len(raw))
	for _, entry := range raw {
		listPrice := entry.baseRate.Mul(mult)
		contractedPrice := listPrice.Mul(disc)
		inputs = append(inputs, mergedPriceLineInput{
			component:       entry.component,
			tier:            entry.tier,
			unit:            entry.unit,
			quantity:        entry.quantity,
			listPrice:       listPrice,
			contractedPrice: contractedPrice,
		})
	}
	return mergeAndSortPriceLines(inputs)
}

func mergeAndSortPriceLines(inputs []mergedPriceLineInput) []PriceLine {
	type lineKey struct {
		component  string
		hasTier    bool
		tier       string
		unit       string
		listPrice  string
		contracted string
	}
	type group struct {
		component      string
		tier           *string
		unit           string
		quantity       decimal.Decimal
		listPrice      decimal.Decimal
		listText       string
		contractedText string
	}
	byKey := make(map[lineKey]*group, len(inputs))
	groups := make([]*group, 0, len(inputs))
	for _, entry := range inputs {
		listText := trimDecimalString(entry.listPrice)
		contractedText := trimDecimalString(entry.contractedPrice)
		key := lineKey{
			component:  entry.component,
			hasTier:    entry.tier != nil,
			unit:       entry.unit,
			listPrice:  listText,
			contracted: contractedText,
		}
		if entry.tier != nil {
			key.tier = *entry.tier
		}
		if existing, ok := byKey[key]; ok {
			existing.quantity = existing.quantity.Add(entry.quantity)
			continue
		}
		g := &group{
			component:      entry.component,
			tier:           entry.tier,
			unit:           entry.unit,
			quantity:       entry.quantity,
			listPrice:      entry.listPrice,
			listText:       listText,
			contractedText: contractedText,
		}
		byKey[key] = g
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b *group) int {
		if c := cmp.Compare(a.component, b.component); c != 0 {
			return c
		}
		if c := compareOptionalString(a.tier, b.tier); c != 0 {
			return c
		}
		return a.listPrice.Cmp(b.listPrice)
	})
	out := make([]PriceLine, len(groups))
	for i, g := range groups {
		out[i] = PriceLine{
			Component:           g.component,
			Tier:                g.tier,
			Unit:                g.unit,
			Quantity:            trimDecimalString(g.quantity),
			ListUnitPrice:       g.listText,
			ContractedUnitPrice: g.contractedText,
		}
	}
	return out
}

func compareOptionalString(a, b *string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return cmp.Compare(*a, *b)
	}
}

func decimalFromScaled(v *big.Int, scale int32) decimal.Decimal {
	return decimal.NewFromBigInt(new(big.Int).Set(v), -scale)
}

func trimDecimalString(d decimal.Decimal) string {
	if d.IsZero() {
		return "0"
	}
	s := d.String()
	if !strings.ContainsRune(s, '.') {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

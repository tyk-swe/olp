package billing

import (
	"strings"

	"github.com/shopspring/decimal"
)

// lineSum accumulates one (day, line_item) across project/workspace groups
// and across pages.
type lineSum struct {
	amount   decimal.Decimal
	currency string
}

type reportAccumulator map[string]map[string]lineSum

// ensureDay retains empty buckets, including days whose rows are all filtered.
func (r reportAccumulator) ensureDay(day string) {
	if r[day] == nil {
		r[day] = map[string]lineSum{}
	}
}

// add chooses the first nonempty vendor label and keeps the last nonempty
// currency, preserving the report's order across pages.
func (r reportAccumulator) add(day string, amount decimal.Decimal, currency string, labels ...*string) {
	r.ensureDay(day)
	lineItem := "unspecified"
	for _, label := range labels {
		if label != nil && *label != "" {
			lineItem = truncateLineItem(*label)
			break
		}
	}
	sum := r[day][lineItem]
	sum.amount = sum.amount.Add(amount)
	if currency != "" {
		sum.currency = upperCurrency(currency)
	}
	r[day][lineItem] = sum
}

// truncateLineItem bounds a vendor label to the stored column's 256
// characters.
func truncateLineItem(value string) string {
	runes := []rune(value)
	if len(runes) <= 256 {
		return value
	}
	return string(runes[:256])
}

// upperCurrency normalizes the vendor's currency tag for storage.
func upperCurrency(currency string) string {
	if len(currency) > 3 {
		currency = currency[:3]
	}
	return strings.ToUpper(currency)
}

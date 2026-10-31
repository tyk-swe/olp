package billing

import (
	"strings"

	"github.com/shopspring/decimal"
)

type lineSum struct {
	amount decimal.Decimal
}

type lineKey struct {
	item     string
	currency string
}

type reportAccumulator map[string]map[lineKey]lineSum

// ensureDay retains empty buckets, including days whose rows are all filtered.
func (r reportAccumulator) ensureDay(day string) {
	if r[day] == nil {
		r[day] = map[lineKey]lineSum{}
	}
}

func (r reportAccumulator) add(day string, amount decimal.Decimal, currency string, labels ...*string) {
	r.ensureDay(day)
	lineItem := "unspecified"
	for _, label := range labels {
		if label != nil && *label != "" {
			lineItem = truncateLineItem(*label)
			break
		}
	}
	key := lineKey{item: lineItem, currency: upperCurrency(currency)}
	r[day][key] = lineSum{amount: r[day][key].amount.Add(amount)}
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

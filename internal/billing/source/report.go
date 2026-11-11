package source

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/pricingpolicy"
)

type Period struct {
	Days     Days
	Revision *Revision
}

type Revision struct {
	PeriodStart time.Time
	ID          string
	OrderingAt  time.Time
	Replaces    *Window
}

type Window struct {
	From time.Time
	To   time.Time
}

func (p Period) Coverage() (from, through *time.Time) {
	if p.Revision != nil && p.Revision.Replaces != nil {
		window := *p.Revision.Replaces
		if !window.From.Before(window.To) {
			return nil, nil
		}
		last := window.To.AddDate(0, 0, -1)
		return &window.From, &last
	}
	days := p.Days.Days()
	if len(days) == 0 {
		return nil, nil
	}
	return &days[0], &days[len(days)-1]
}

type Line struct {
	Item     string
	Currency string
	Amount   decimal.Decimal
}

type lineKey struct {
	item     string
	currency string
}

type Days struct {
	lines map[time.Time]map[lineKey]decimal.Decimal
}

func newDays() Days {
	return Days{lines: map[time.Time]map[lineKey]decimal.Decimal{}}
}

func day(t time.Time) time.Time {
	return t.UTC().Truncate(24 * time.Hour)
}

func (d *Days) include(t time.Time) {
	if d.lines[day(t)] == nil {
		d.lines[day(t)] = map[lineKey]decimal.Decimal{}
	}
}

func (d Days) has(t time.Time) bool {
	_, ok := d.lines[day(t)]
	return ok
}

func (d *Days) add(t time.Time, label, currency string, amount decimal.Decimal) error {
	code, ok := currencyCode(currency)
	if !ok {
		return fail(CodeResponse, errors.New("vendor reported an invalid currency"))
	}
	d.include(t)
	key := lineKey{item: lineItem(label), currency: code}
	d.lines[day(t)][key] = d.lines[day(t)][key].Add(amount)
	return nil
}

func (d Days) size(t time.Time) int {
	return len(d.lines[day(t)])
}

func (d Days) lineCount() int {
	total := 0
	for _, lines := range d.lines {
		total += len(lines)
	}
	return total
}

func (d Days) Days() []time.Time {
	return slices.SortedFunc(maps.Keys(d.lines), time.Time.Compare)
}

func (d Days) Lines(t time.Time) []Line {
	lines := d.lines[day(t)]
	keys := slices.SortedFunc(maps.Keys(lines), func(a, b lineKey) int {
		return cmp.Or(cmp.Compare(a.item, b.item), cmp.Compare(a.currency, b.currency))
	})
	return slices.Collect(func(yield func(Line) bool) {
		for _, key := range keys {
			if !yield(Line{Item: key.item, Currency: key.currency, Amount: lines[key]}) {
				return
			}
		}
	})
}

func currencyCode(currency string) (string, bool) {
	code := strings.ToUpper(currency)
	return code, len(currency) == 3 && pricingpolicy.ValidCurrency(code)
}

func lineItem(label string) string {
	runes := []rune(cmp.Or(label, "unspecified"))
	if len(runes) <= 256 {
		return string(runes)
	}
	digest := sha256.Sum256([]byte(label))
	return string(runes[:191]) + "-" + hex.EncodeToString(digest[:])
}

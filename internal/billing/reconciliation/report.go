package reconciliation

import (
	"bytes"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type reason = contract.ReconciliationReportItemsCaptureRateUnavailableReason

type importStatus = struct {
	LastErrorCode nullable.Nullable[string]    `json:"last_error_code,omitempty"`
	LastSuccessAt nullable.Nullable[time.Time] `json:"last_success_at,omitempty"`
}

type item = struct {
	AttributedCost               string                          `json:"attributed_cost"`
	CaptureRate                  nullable.Nullable[string]       `json:"capture_rate"`
	CaptureRateUnavailableReason nullable.Nullable[reason]       `json:"capture_rate_unavailable_reason"`
	Day                          openapi_types.Date              `json:"day"`
	ExcludedCallerAttempts       int                             `json:"excluded_caller_attempts"`
	Import                       nullable.Nullable[importStatus] `json:"import"`
	IncompleteAttempts           int                             `json:"incomplete_attempts"`
	InvoicedCost                 nullable.Nullable[string]       `json:"invoiced_cost"`
	InvoicedCurrency             nullable.Nullable[string]       `json:"invoiced_currency"`
	PricedAttempts               int                             `json:"priced_attempts"`
	ProviderId                   openapi_types.UUID              `json:"provider_id"`
	Provisional                  bool                            `json:"provisional"`
	UnpricedAttempts             int                             `json:"unpriced_attempts"`
	UnpricedShare                nullable.Nullable[string]       `json:"unpriced_share"`
}

type total = struct {
	AttributedCost               string                    `json:"attributed_cost"`
	AttributedCostCovered        string                    `json:"attributed_cost_covered"`
	CaptureRate                  nullable.Nullable[string] `json:"capture_rate"`
	CaptureRateUnavailableReason nullable.Nullable[string] `json:"capture_rate_unavailable_reason"`
	InvoicedCost                 nullable.Nullable[string] `json:"invoiced_cost"`
	ProviderId                   openapi_types.UUID        `json:"provider_id"`
	UnpricedShare                nullable.Nullable[string] `json:"unpriced_share"`
}

type inputs struct {
	window   Range
	imports  []providerImport
	coverage []coveredDay
	invoiced []invoicedDay
	spend    []daySpend
	currency string
	today    time.Time
}

type ledger struct {
	window   Range
	coverage map[cellKey]coveredDay
	invoiced map[cellKey]invoicedDay
	spend    map[cellKey]daySpend
	currency string
	today    time.Time
}

type day struct {
	date        time.Time
	spend       daySpend
	covered     bool
	invoice     invoicedDay
	lastSuccess *time.Time
}

type summary struct {
	attributed, attributedCovered, invoiced decimal.Decimal
	priced, unpriced                        int64
	covered, currencyMismatch               bool
}

func build(in inputs) contract.ReconciliationReport {
	l := ledger{
		window:   in.window,
		coverage: index(in.coverage),
		invoiced: index(in.invoiced),
		spend:    index(in.spend),
		currency: in.currency,
		today:    in.today,
	}
	imports := slices.SortedFunc(slices.Values(in.imports), func(a, b providerImport) int {
		return bytes.Compare(a.ProviderID[:], b.ProviderID[:])
	})
	items := []item{}
	totals := make([]total, 0, len(imports))
	for _, imp := range imports {
		days := l.days(imp)
		status := importOf(imp)
		for _, d := range days {
			items = append(items, l.item(imp.ProviderID, status, d))
		}
		totals = append(totals, summarize(days, l.currency).total(imp.ProviderID))
	}
	return contract.ReconciliationReport{
		Start:    openapi_types.Date{Time: in.window.Start},
		End:      openapi_types.Date{Time: in.window.End},
		Currency: in.currency,
		Items:    items,
		Totals:   totals,
	}
}

func index[T interface{ key() cellKey }](rows []T) map[cellKey]T {
	indexed := make(map[cellKey]T, len(rows))
	for _, row := range rows {
		indexed[row.key()] = row
	}
	return indexed
}

func (l ledger) days(imp providerImport) []day {
	days := []day{}
	for date := l.window.Start; date.Before(l.window.End); date = date.AddDate(0, 0, 1) {
		if d, ok := l.day(imp, date); ok {
			days = append(days, d)
		}
	}
	return days
}

func (l ledger) day(imp providerImport, date time.Time) (day, bool) {
	key := cellKey{ProviderID: imp.ProviderID, Day: date}
	spend, spent := l.spend[key]
	invoice, invoiced := l.invoiced[key]
	coverage, covered := l.coverage[key]
	switch {
	case covered:
		return day{date: date, spend: spend, covered: true, invoice: invoice, lastSuccess: coverage.LastSuccessAt}, true
	case spent || invoiced:
		return day{date: date, spend: spend, lastSuccess: imp.LastSuccessAt}, true
	}
	return day{}, false
}

func (l ledger) item(provider uuid.UUID, status nullable.Nullable[importStatus], d day) item {
	rate, reason := d.captureRate(l.currency)
	return item{
		AttributedCost:               d.spend.Attributed.String(),
		CaptureRate:                  rate,
		CaptureRateUnavailableReason: reason,
		Day:                          openapi_types.Date{Time: d.date},
		ExcludedCallerAttempts:       int(d.spend.ExcludedCallerAttempts),
		Import:                       status,
		IncompleteAttempts:           int(d.spend.IncompleteAttempts),
		InvoicedCost:                 d.invoicedCost(),
		InvoicedCurrency:             nonEmpty(strings.Join(d.invoice.Currencies, ",")),
		PricedAttempts:               int(d.spend.PricedAttempts),
		ProviderId:                   provider,
		Provisional:                  d.provisional(l.today),
		UnpricedAttempts:             int(d.spend.UnpricedAttempts),
		UnpricedShare:                unpricedShare(d.spend.PricedAttempts, d.spend.UnpricedAttempts),
	}
}

func importOf(imp providerImport) nullable.Nullable[importStatus] {
	if !imp.Active {
		return nullable.NewNullNullable[importStatus]()
	}
	return nullable.NewNullableWithValue(importStatus{
		LastErrorCode: nullableOf(imp.LastErrorCode),
		LastSuccessAt: nullableOf(imp.LastSuccessAt),
	})
}

func (d day) unavailableReason(currency string) (reason, bool) {
	switch {
	case !d.covered:
		return contract.ReconciliationReportItemsCaptureRateUnavailableReasonNotImported, true
	case d.invoice.Amount.IsZero() && len(d.invoice.Currencies) < 2:
		return contract.ReconciliationReportItemsCaptureRateUnavailableReasonZeroInvoice, true
	case !slices.Equal(d.invoice.Currencies, []string{currency}):
		return contract.ReconciliationReportItemsCaptureRateUnavailableReasonCurrencyMismatch, true
	}
	return "", false
}

func (d day) captureRate(currency string) (nullable.Nullable[string], nullable.Nullable[reason]) {
	if reason, unavailable := d.unavailableReason(currency); unavailable {
		return nullable.NewNullNullable[string](), nullable.NewNullableWithValue(reason)
	}
	return nullable.NewNullableWithValue(ratio(d.spend.Attributed, d.invoice.Amount)), nullable.NewNullNullable[reason]()
}

func (d day) invoicedCost() nullable.Nullable[string] {
	if !d.covered {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(d.invoice.Amount.String())
}

func (d day) provisional(today time.Time) bool {
	return !d.date.Before(today.Add(-48*time.Hour)) || d.lastSuccess == nil || d.lastSuccess.Before(d.date.Add(30*time.Hour))
}

func summarize(days []day, currency string) summary {
	var s summary
	for _, d := range days {
		s.attributed = s.attributed.Add(d.spend.Attributed)
		s.priced += d.spend.PricedAttempts
		s.unpriced += d.spend.UnpricedAttempts
		if !d.covered {
			continue
		}
		reason, unavailable := d.unavailableReason(currency)
		s.covered = true
		s.attributedCovered = s.attributedCovered.Add(d.spend.Attributed)
		s.invoiced = s.invoiced.Add(d.invoice.Amount)
		s.currencyMismatch = s.currencyMismatch ||
			unavailable && reason == contract.ReconciliationReportItemsCaptureRateUnavailableReasonCurrencyMismatch
	}
	return s
}

func (s summary) total(provider uuid.UUID) total {
	rate, reason := s.captureRate()
	return total{
		AttributedCost:               s.attributed.String(),
		AttributedCostCovered:        s.attributedCovered.String(),
		CaptureRate:                  rate,
		CaptureRateUnavailableReason: reason,
		InvoicedCost:                 s.invoicedCost(),
		ProviderId:                   provider,
		UnpricedShare:                unpricedShare(s.priced, s.unpriced),
	}
}

func (s summary) captureRate() (nullable.Nullable[string], nullable.Nullable[string]) {
	switch {
	case !s.covered:
		return nullable.NewNullNullable[string](), nullable.NewNullableWithValue(string(contract.ReconciliationReportItemsCaptureRateUnavailableReasonNotImported))
	case s.currencyMismatch:
		return nullable.NewNullNullable[string](), nullable.NewNullableWithValue(string(contract.ReconciliationReportItemsCaptureRateUnavailableReasonCurrencyMismatch))
	case s.invoiced.IsZero():
		return nullable.NewNullNullable[string](), nullable.NewNullableWithValue(string(contract.ReconciliationReportItemsCaptureRateUnavailableReasonZeroInvoice))
	}
	return nullable.NewNullableWithValue(ratio(s.attributedCovered, s.invoiced)), nullable.NewNullNullable[string]()
}

func (s summary) invoicedCost() nullable.Nullable[string] {
	if !s.covered {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(s.invoiced.String())
}

func unpricedShare(priced, unpriced int64) nullable.Nullable[string] {
	if priced+unpriced == 0 {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(ratio(decimal.NewFromInt(unpriced), decimal.NewFromInt(priced+unpriced)))
}

func ratio(numerator, denominator decimal.Decimal) string {
	return numerator.Div(denominator).Round(6).String()
}

func nonEmpty(value string) nullable.Nullable[string] {
	if value == "" {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(value)
}

func nullableOf[T any](value *T) nullable.Nullable[T] {
	if value == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*value)
}

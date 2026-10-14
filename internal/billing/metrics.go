package billing

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

// WriteMetrics emits the billing import's DB-backed series the way
// export.WriteMetrics emits the delivery plane's.
func WriteMetrics(ctx context.Context, q access.Queryer, importer *Importer, body *strings.Builder) {
	rates, err := captureRates(ctx, q)
	if err == nil && len(rates) > 0 {
		body.WriteString("# HELP olp_billing_capture_rate Attributed-to-invoiced spend ratio per provider over the last 7 closed covered days.\n# TYPE olp_billing_capture_rate gauge\n")
		for provider, rate := range rates {
			body.WriteString("olp_billing_capture_rate{provider=" + strconv.Quote(provider) + "} " + rate + "\n")
		}
	}
	if importer != nil {
		metrics := importer.Metrics()
		if len(metrics) > 0 {
			body.WriteString("# HELP olp_billing_imports_total Billing imports this process ran, by provider and outcome.\n# TYPE olp_billing_imports_total counter\n")
			providers := make([]string, 0, len(metrics))
			for key := range metrics {
				providers = append(providers, key[0]+" "+key[1])
			}
			sort.Strings(providers)
			for _, item := range providers {
				parts := strings.SplitN(item, " ", 2)
				body.WriteString("olp_billing_imports_total{provider=" + strconv.Quote(parts[0]) + ",outcome=" + strconv.Quote(parts[1]) + "} " + strconv.FormatInt(metrics[[2]string{parts[0], parts[1]}], 10) + "\n")
			}
		}
	}
}

// captureRates computes each import's attributed-to-invoiced ratio over the
// last 7 closed UTC days the import covers, or omits the provider when the
// window is uncovered, zero-invoiced or off-currency.
func captureRates(ctx context.Context, q access.Queryer) (map[string]string, error) {
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -7)
	var providerIDs []string
	rows, err := q.Query(ctx, `SELECT provider_id::text FROM olp.provider_billing_imports
		WHERE enabled AND imported_from <= $1::date AND imported_through >= $2::date`,
		start.Format(dayFormat), end.AddDate(0, 0, -1).Format(dayFormat))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		providerIDs = append(providerIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(providerIDs) == 0 {
		return nil, err
	}
	var pricingCurrency string
	if err := q.QueryRow(ctx, `SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton`).Scan(&pricingCurrency); err != nil {
		return nil, err
	}
	// The window is covered: the import bound contains it entirely.
	type agg struct {
		attributed, invoiced decimal.Decimal
		covered              bool
	}
	byProvider := map[string]*agg{}
	for _, id := range providerIDs {
		byProvider[id] = &agg{invoiced: decimal.Zero, attributed: decimal.Zero}
	}
	invoicedRows, err := q.Query(ctx, `SELECT provider_id::text,sum(amount)::text,string_agg(DISTINCT btrim(currency),',')
		FROM olp.provider_invoiced_costs
		WHERE provider_id=ANY($1::uuid[]) AND day>=$2::date AND day<$3::date
		GROUP BY provider_id`, providerIDs, start.Format(dayFormat), end.Format(dayFormat))
	if err != nil {
		return nil, err
	}
	defer invoicedRows.Close()
	mismatch := map[string]bool{}
	for invoicedRows.Next() {
		var provider, amount, currencies string
		if err := invoicedRows.Scan(&provider, &amount, &currencies); err != nil {
			return nil, err
		}
		if v, err := decimal.NewFromString(amount); err == nil {
			byProvider[provider].invoiced = v
		}
		mismatch[provider] = currencies != pricingCurrency
	}
	if err := invoicedRows.Err(); err != nil {
		return nil, err
	}
	spend, err := usage.ReconciliationSpend(ctx, q, start, end, providerIDs)
	if err != nil {
		return nil, err
	}
	for _, row := range spend {
		if v, err := decimal.NewFromString(row.AttributedCost); err == nil {
			a := byProvider[row.ProviderID]
			a.attributed = a.attributed.Add(v)
		}
	}
	rates := map[string]string{}
	for provider, a := range byProvider {
		if a.invoiced.IsZero() || mismatch[provider] {
			continue
		}
		rates[provider] = a.attributed.Div(a.invoiced).Round(6).String()
	}
	return rates, nil
}

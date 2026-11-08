package billing

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/metric"
	"github.com/tyk-swe/olp/internal/usage"
)

var (
	captureRate = metric.Gauge("olp_billing_capture_rate", "Attributed-to-invoiced spend ratio per provider over the last 7 closed covered days.", "provider")
	imports     = metric.Counter("olp_billing_imports_total", "Billing imports this process ran, by provider and outcome.", "provider", "outcome")
)

func Metrics(ctx context.Context, q access.Queryer, importer *Importer) []prometheus.Metric {
	rates, _ := captureRates(ctx, q)
	var metrics []prometheus.Metric
	for provider, rate := range rates {
		metrics = append(metrics, captureRate.Sample(rate, provider))
	}
	if importer == nil {
		return metrics
	}
	for key, n := range importer.Metrics() {
		metrics = append(metrics, imports.Sample(float64(n), key[0], key[1]))
	}
	return metrics
}

// captureRates computes each import's attributed-to-invoiced ratio over the
// last 7 closed UTC days the import covers, or omits the provider when the
// window is uncovered, zero-invoiced or off-currency.
func captureRates(ctx context.Context, q access.Queryer) (map[string]float64, error) {
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
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton), 'USD')`).Scan(&pricingCurrency); err != nil {
		return nil, err
	}
	// The window is covered: the import bound contains it entirely.
	type agg struct {
		attributed, invoiced decimal.Decimal
	}
	byProvider := map[string]*agg{}
	invoicedRows, err := q.Query(ctx, `SELECT provider_id::text,sum(amount)::text
		FROM olp.provider_invoiced_costs
		WHERE provider_id=ANY($1::uuid[]) AND day>=$2::date AND day<$3::date
		GROUP BY provider_id
		HAVING sum(amount) <> 0 AND string_agg(DISTINCT btrim(currency),',') = $4`,
		providerIDs, start.Format(dayFormat), end.Format(dayFormat), pricingCurrency)
	if err != nil {
		return nil, err
	}
	defer invoicedRows.Close()
	var eligibleProviders []string
	for invoicedRows.Next() {
		var provider, amount string
		if err := invoicedRows.Scan(&provider, &amount); err != nil {
			return nil, err
		}
		if v, err := decimal.NewFromString(amount); err == nil {
			byProvider[provider] = &agg{invoiced: v, attributed: decimal.Zero}
			eligibleProviders = append(eligibleProviders, provider)
		}
	}
	if err := invoicedRows.Err(); err != nil {
		return nil, err
	}
	spend, err := usage.ReconciliationSpend(ctx, q, start, end, eligibleProviders)
	if err != nil {
		return nil, err
	}
	for _, row := range spend {
		if v, err := decimal.NewFromString(row.AttributedCost); err == nil {
			a := byProvider[row.ProviderID]
			a.attributed = a.attributed.Add(v)
		}
	}
	rates := make(map[string]float64, len(byProvider))
	for provider, a := range byProvider {
		rates[provider] = a.attributed.Div(a.invoiced).Round(6).InexactFloat64()
	}
	return rates, nil
}

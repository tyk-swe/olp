package reconciliation

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/metric"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
)

var captureRate = metric.Gauge("olp_billing_capture_rate", "Attributed-to-invoiced spend ratio per provider over the last 7 closed covered days.", "provider")

type windowInvoice struct {
	ProviderID uuid.UUID
	Amount     decimal.Decimal
}

func CaptureRates(ctx context.Context, q access.Queryer, now time.Time) ([]prometheus.Metric, error) {
	end := now.UTC().Truncate(24 * time.Hour)
	window := Range{Start: end.AddDate(0, 0, -7), End: end}
	currency, err := pricebook.InstallationCurrency(ctx, q)
	if err != nil {
		return nil, err
	}
	invoices, err := windowInvoices(ctx, q, window, currency)
	if err != nil {
		return nil, err
	}
	providers := make([]uuid.UUID, len(invoices))
	for i, invoice := range invoices {
		providers[i] = invoice.ProviderID
	}
	spend, err := attributedSpend(ctx, q, providers, window)
	if err != nil {
		return nil, err
	}
	attributed := map[uuid.UUID]decimal.Decimal{}
	for _, row := range spend {
		attributed[row.ProviderID] = attributed[row.ProviderID].Add(row.Attributed)
	}
	samples := make([]prometheus.Metric, len(invoices))
	for i, invoice := range invoices {
		rate := attributed[invoice.ProviderID].Div(invoice.Amount).Round(6).InexactFloat64()
		samples[i] = captureRate.Sample(rate, invoice.ProviderID.String())
	}
	return samples, nil
}

func windowInvoices(ctx context.Context, q access.Queryer, window Range, currency string) ([]windowInvoice, error) {
	rows, err := q.Query(ctx, `SELECT ic.provider_id, sum(ic.amount)
		FROM olp.provider_invoiced_costs ic
		JOIN olp.provider_billing_imports bi ON bi.provider_id=ic.provider_id
		WHERE bi.enabled AND bi.imported_from<=$1::date AND bi.imported_through>=$2::date
		  AND ic.day>=$1::date AND ic.day<$3::date
		GROUP BY ic.provider_id
		HAVING sum(ic.amount)<>0 AND count(DISTINCT btrim(ic.currency))=1 AND min(btrim(ic.currency))=$4`,
		window.Start, window.End.AddDate(0, 0, -1), window.End, currency)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[windowInvoice])
}

package reconciliation

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestReportReasonPrecedenceAndProvisionalDays(t *testing.T) {
	provider := uuid.MustParse("6f1c2a4e-8d3b-4c5a-9e7f-0a1b2c3d4e5f")
	date := func(day int) time.Time { return time.Date(2026, time.November, day, 0, 0, 0, 0, time.UTC) }
	key := func(day int) cellKey { return cellKey{ProviderID: provider, Day: date(day)} }
	imported := date(10).Add(12 * time.Hour)
	covered := func(day int) coveredDay { return coveredDay{cellKey: key(day), LastSuccessAt: &imported} }
	report := build(inputs{
		window:   Range{Start: date(1), End: date(11)},
		imports:  []providerImport{{ProviderID: provider, Active: true}},
		coverage: []coveredDay{covered(1), covered(2), covered(4), covered(8)},
		invoiced: []invoicedDay{
			{cellKey: key(1), Amount: decimal.Zero, Currencies: []string{"EUR", "USD"}},
			{cellKey: key(4), Amount: decimal.RequireFromString("3"), Currencies: []string{"USD"}},
			{cellKey: key(8), Amount: decimal.RequireFromString("1"), Currencies: []string{"USD"}},
		},
		spend: []daySpend{
			{cellKey: key(3), Attributed: decimal.RequireFromString("1.5"), PricedAttempts: 1, UnpricedAttempts: 2},
			{cellKey: key(4), Attributed: decimal.RequireFromString("1"), PricedAttempts: 1},
		},
		currency: "USD",
		today:    date(10),
	})
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Items  []map[string]any `json:"items"`
		Totals []map[string]any `json:"totals"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	fields := func(row map[string]any, names ...string) string {
		values := make([]any, len(names))
		for i, name := range names {
			values[i] = row[name]
		}
		return fmt.Sprint(values)
	}
	got := make([]string, 0, len(wire.Items)+len(wire.Totals))
	for _, item := range wire.Items {
		got = append(got, fields(item, "day", "invoiced_cost", "capture_rate", "capture_rate_unavailable_reason", "provisional"))
	}
	for _, total := range wire.Totals {
		got = append(got, fields(total, "attributed_cost", "attributed_cost_covered", "invoiced_cost", "capture_rate", "capture_rate_unavailable_reason", "unpriced_share"))
	}
	want := []string{
		"[2026-11-01 0 <nil> currency_mismatch false]",
		"[2026-11-02 0 <nil> zero_invoice false]",
		"[2026-11-03 <nil> <nil> not_imported true]",
		"[2026-11-04 3 0.333333 <nil> false]",
		"[2026-11-08 1 0 <nil> true]",
		"[2.5 1 4 <nil> currency_mismatch 0.5]",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("report:\n%v\nwant\n%v", got, want)
	}
}

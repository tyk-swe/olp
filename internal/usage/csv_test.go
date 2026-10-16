package usage

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func csvRows(t *testing.T, data []byte, err error) []map[string]string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]string{}
	for _, record := range records[1:] {
		row := map[string]string{}
		for i, field := range records[0] {
			row[field] = record[i]
		}
		rows = append(rows, row)
	}
	return rows
}

func TestUsageCSVPreservesCoverageAndExactTotals(t *testing.T) {
	cost, currency := "10000000000000000000.000000001", "USD"
	filters := Filters{Start: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)}
	totals := Totals{RequestCount: 3, InputTokens: "90071992547409930", EstimatedCost: &cost, Currency: &currency, UnpricedCount: 1, IncompleteCount: 2}
	coverage := Coverage{Approximate: true, ExcludedBoundaries: 2}
	summary := Summary{Totals: totals, Coverage: coverage, GapEvents: 7, UncertainGapCount: 1}
	breakdown := Breakdown{Coverage: coverage, Items: []BreakdownItem{{Dimension: "@session", Totals: totals}}}
	data, err := UsageCSV(filters, DimensionSession, summary, breakdown)
	rows := csvRows(t, data, err)
	if len(rows) != 2 || rows[0]["row_type"] != "summary" || rows[1]["dimension_value"] != "'@session" {
		t.Fatalf("rows %+v", rows)
	}
	for _, row := range rows {
		if row["estimated_cost"] != cost || row["input_tokens"] != totals.InputTokens || row["complete"] != "false" ||
			row["approximate"] != "true" || row["range_complete"] != "false" || row["unpriced_count"] != "1" ||
			row["excluded_partial_aggregate_boundaries"] != "2" || row["request_metadata_gap_events"] != "7" {
			t.Fatalf("coverage or exact totals changed: %+v", row)
		}
	}
}

func TestCSVRejectsOversizeAndNeutralizesSpreadsheetFormulas(t *testing.T) {
	if _, err := RequestCSV(make([]RequestSummary, 10001)); err == nil {
		t.Fatal("oversized record count accepted")
	}
	if _, err := RequestCSV([]RequestSummary{{Route: strings.Repeat("x", csvMaxBytes)}}); err == nil {
		t.Fatal("oversized bytes accepted")
	}
	for _, value := range []string{"=1", "+1", "-1", "@formula", " \uFEFF=1", "\t1", "\r1", "\n1"} {
		if csvCell(value) != "'"+value {
			t.Fatalf("unsafe value unchanged: %q", value)
		}
	}
	if csvCell("route-1") != "route-1" {
		t.Fatal("ordinary text changed")
	}
}

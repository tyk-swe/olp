package notifications

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/usage/reports"
)

func spikeBaseline(cost string, days int) []decimal.Decimal {
	value := decimal.RequireFromString(cost)
	baseline := make([]decimal.Decimal, days)
	for i := range baseline {
		baseline[i] = value
	}
	return baseline
}

func TestSpendSpikeDecision(t *testing.T) {
	t.Parallel()
	config, err := resolveSpendSpike(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		actual   string
		baseline []decimal.Decimal
		want     bool
	}{
		{"material spike", "150", spikeBaseline("20", 7), true},
		{"exact thresholds", "30", spikeBaseline("20", 7), true},
		{"percent without absolute", "15", spikeBaseline("10", 7), false},
		{"absolute without percent", "210", spikeBaseline("200", 7), false},
		{"insufficient history", "150", spikeBaseline("20", 6), false},
		{"zero baseline", "100", spikeBaseline("0", 7), false},
		{"below baseline", "10", spikeBaseline("20", 7), false},
		{"mixed baseline mean", "45", []decimal.Decimal{
			decimal.RequireFromString("10"), decimal.RequireFromString("20"),
			decimal.RequireFromString("30"), decimal.RequireFromString("10"),
			decimal.RequireFromString("20"), decimal.RequireFromString("30"),
			decimal.RequireFromString("20"),
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			days := spikeDays{state: dayQualified, actual: decimal.RequireFromString(test.actual), baseline: test.baseline}
			evidence, fired := config.decide(days)
			if fired != test.want {
				t.Fatalf("fired = %v, want %v (evidence %+v)", fired, test.want, evidence)
			}
			if !fired {
				return
			}
			if evidence.daysUsed != len(test.baseline) || !evidence.increase.IsPositive() || !evidence.baseline.IsPositive() {
				t.Fatalf("evidence = %+v, want %d days and positive amounts", evidence, len(test.baseline))
			}
		})
	}
}

func TestSpendSpikeEvidence(t *testing.T) {
	t.Parallel()
	config, err := resolveSpendSpike(nil)
	if err != nil {
		t.Fatal(err)
	}
	days := spikeDays{state: dayQualified, actual: decimal.RequireFromString("150"), baseline: spikeBaseline("20", 7)}
	evidence, fired := config.decide(days)
	if !fired {
		t.Fatal("want fired")
	}
	if evidence.baseline.String() != "20" || evidence.increase.String() != "130" || evidence.percent.String() != "650" {
		t.Fatalf("evidence = %+v, want baseline 20, increase 130, percent 650", evidence)
	}
}

func TestQualifyDay(t *testing.T) {
	t.Parallel()
	cost, negative := "12.50", "-1"
	for _, test := range []struct {
		name   string
		totals reports.Totals
		want   dayState
	}{
		{"absent", reports.Totals{}, dayAbsent},
		{"missing cost", reports.Totals{RequestCount: 3}, dayIncomplete},
		{"unpriced", reports.Totals{RequestCount: 3, EstimatedCost: &cost, UnpricedCount: 1}, dayIncomplete},
		{"incomplete usage", reports.Totals{RequestCount: 3, EstimatedCost: &cost, IncompleteCount: 1}, dayIncomplete},
		{"negative cost", reports.Totals{RequestCount: 3, EstimatedCost: &negative}, dayIncomplete},
		{"qualified", reports.Totals{RequestCount: 3, EstimatedCost: &cost}, dayQualified},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			state, value, _ := qualifyDay(test.totals)
			if state != test.want {
				t.Fatalf("state = %v, want %v", state, test.want)
			}
			if state == dayQualified && value.String() != "12.5" {
				t.Fatalf("cost = %s, want 12.5", value)
			}
		})
	}
}

func TestExtractDaysScopesBaselineToActualCurrency(t *testing.T) {
	t.Parallel()
	window := spikeWindowFor(time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC), 14)
	point := func(day int, cost, currency string, unpriced int64) reports.Point {
		return reports.Point{
			Bucket: time.Date(2026, time.March, day, 0, 0, 0, 0, time.UTC),
			Totals: reports.Totals{RequestCount: 2, EstimatedCost: &cost, Currency: &currency, UnpricedCount: unpriced},
		}
	}
	series := reports.Series{Items: []reports.Point{
		point(14, "150", "USD", 0),
		point(13, "20", "USD", 0),
		point(12, "20", "USD", 1),
		point(11, "20", "EUR", 0),
	}}
	days := extractDays(series, window)
	if days.state != dayQualified || days.actual.String() != "150" {
		t.Fatalf("days = %+v, want qualified actual 150", days)
	}
	if len(days.baseline) != 1 || days.baseline[0].String() != "20" {
		t.Fatalf("baseline = %v, want one USD priced day of 20", days.baseline)
	}
}

func TestSpendSpikeRuleRequiresProject(t *testing.T) {
	t.Parallel()
	input := ruleInput{Name: "spike", Event: contract.NotificationEventSpendSpike, DestinationID: "0d2f5f4e-2b8a-4a51-8f3c-5c7a3e9b1d20"}
	if _, err := parseRule(input, nil); err == nil {
		t.Fatal("installation-wide spike rule accepted, want rejection")
	}
	project := "3a5f21c8-7d44-4e91-8b2c-9d0e6f31a507"
	if _, err := parseRule(input, &project); err != nil {
		t.Fatalf("project spike rule rejected: %v", err)
	}
}

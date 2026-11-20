package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/usage/reports"
)

type spikeWindow struct {
	baselineStart, dayStart, dayEnd time.Time
}

type dayState int

const (
	dayAbsent dayState = iota
	dayIncomplete
	dayQualified
)

type spikeDays struct {
	state    dayState
	actual   decimal.Decimal
	currency *string
	baseline []decimal.Decimal
}

func (c spendSpikeConfig) claim(ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) (int, error) {
	if rule.project == nil {
		return 0, nil
	}
	window := spikeWindowFor(env.now, c.BaselineDays)
	if !window.dayEnd.After(rule.createdAt) {
		return 0, nil
	}
	subject := "spend_spike:" + *rule.project
	decided, err := spikeDecided(ctx, tx, rule.id, subject, window.dayEnd)
	if err != nil || decided {
		return 0, err
	}
	days, complete, err := c.readDays(ctx, tx, rule, window, env)
	if err != nil || !complete || days.state == dayIncomplete {
		return 0, err
	}
	if _, err := tx.Exec(ctx, markSpikeEvaluatedSQL, rule.id, subject); err != nil {
		return 0, err
	}
	if days.state == dayAbsent {
		return 0, nil
	}
	evidence, fired := c.decide(days)
	if !fired {
		return 0, nil
	}
	return c.queue(ctx, tx, rule, window, days, evidence, env)
}

func spikeWindowFor(now time.Time, baselineDays int) spikeWindow {
	dayEnd := now.UTC().Truncate(24 * time.Hour)
	dayStart := dayEnd.AddDate(0, 0, -1)
	return spikeWindow{baselineStart: dayStart.AddDate(0, 0, -baselineDays), dayStart: dayStart, dayEnd: dayEnd}
}

func spikeDecided(ctx context.Context, tx pgx.Tx, ruleID, subject string, dayEnd time.Time) (bool, error) {
	var checked time.Time
	err := tx.QueryRow(ctx, spikeEvaluatedSQL, ruleID, subject).Scan(&checked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && !checked.Before(dayEnd), err
}

func (c spendSpikeConfig) readDays(ctx context.Context, tx pgx.Tx, rule signalRule, window spikeWindow, env signalEnv) (spikeDays, bool, error) {
	project := reports.OnlyProjects(*rule.project)
	day := reports.Filters{Start: window.dayStart, End: window.dayEnd, Projects: project}
	completeness, err := reports.ReadCompleteness(ctx, tx, day, env.workerClock())
	if err != nil {
		return spikeDays{}, false, err
	}
	if completeness.GapEvents != 0 || completeness.UncertainGapCount != 0 ||
		!completeness.Coverage.RangeComplete || !completeness.Consumer.Complete() {
		return spikeDays{}, false, nil
	}
	gapDays, err := reports.ReadGapDays(ctx, tx, window.baselineStart, window.dayStart)
	if err != nil {
		return spikeDays{}, false, err
	}
	filters := reports.Filters{Start: window.baselineStart, End: window.dayEnd, Projects: project}
	series, err := reports.ReadSeries(ctx, tx, filters, reports.GranularityDay)
	if err != nil {
		return spikeDays{}, false, err
	}
	return extractDays(series, window, gapDays), true, nil
}

func extractDays(series reports.Series, window spikeWindow, gapDays []time.Time) spikeDays {
	days := spikeDays{baseline: []decimal.Decimal{}}
	for _, point := range series.Items {
		if point.Bucket.UTC().Equal(window.dayStart) {
			days.state, days.actual, days.currency = qualifyDay(point.Totals)
		}
	}
	for _, point := range series.Items {
		bucket := point.Bucket.UTC()
		if bucket.Before(window.baselineStart) || !bucket.Before(window.dayStart) || slices.ContainsFunc(gapDays, bucket.Equal) {
			continue
		}
		if state, cost, currency := qualifyDay(point.Totals); state == dayQualified && sameCurrency(currency, days.currency) {
			days.baseline = append(days.baseline, cost)
		}
	}
	return days
}

func qualifyDay(totals reports.Totals) (dayState, decimal.Decimal, *string) {
	if totals.RequestCount <= 0 {
		return dayAbsent, decimal.Decimal{}, totals.Currency
	}
	if totals.EstimatedCost == nil || totals.UnpricedCount != 0 || totals.IncompleteCount != 0 {
		return dayIncomplete, decimal.Decimal{}, totals.Currency
	}
	cost, err := boundedDecimal(*totals.EstimatedCost, 64, -24)
	if err != nil {
		return dayIncomplete, decimal.Decimal{}, totals.Currency
	}
	return dayQualified, cost, totals.Currency
}

func sameCurrency(a, b *string) bool {
	left, right := "", ""
	if a != nil {
		left = *a
	}
	if b != nil {
		right = *b
	}
	return left == right
}

type spikeEvidence struct {
	baseline, increase, percent decimal.Decimal
	daysUsed                    int
}

func (c spendSpikeConfig) decide(days spikeDays) (spikeEvidence, bool) {
	if len(days.baseline) < c.MinimumHistoryDays {
		return spikeEvidence{}, false
	}
	sum := decimal.Zero
	for _, cost := range days.baseline {
		sum = sum.Add(cost)
	}
	if !sum.IsPositive() {
		return spikeEvidence{}, false
	}
	count := decimal.NewFromInt(int64(len(days.baseline)))
	scaledIncrease := days.actual.Mul(count).Sub(sum)
	if scaledIncrease.Cmp(c.MinimumIncrease.Mul(count)) < 0 {
		return spikeEvidence{}, false
	}
	if scaledIncrease.Mul(decimal.NewFromInt(100)).Cmp(sum.Mul(c.IncreasePercent)) < 0 {
		return spikeEvidence{}, false
	}
	baseline := sum.Div(count)
	return spikeEvidence{
		baseline: baseline,
		increase: days.actual.Sub(baseline),
		percent:  scaledIncrease.Mul(decimal.NewFromInt(100)).Div(sum),
		daysUsed: len(days.baseline),
	}, true
}

func (c spendSpikeConfig) queue(ctx context.Context, tx pgx.Tx, rule signalRule, window spikeWindow, days spikeDays, evidence spikeEvidence, env signalEnv) (int, error) {
	id := env.newID()
	dedupKey := "spend_spike:" + rule.id + ":" + window.dayStart.Format(time.DateOnly)
	usagePath := "/usage?" + url.Values{
		"project_id": {*rule.project},
		"start":      {window.dayStart.Format(time.RFC3339)},
		"end":        {window.dayEnd.Format(time.RFC3339)},
	}.Encode()
	payload, err := json.Marshal(spendSpikePayload{
		envelope:         envelope{Version: 1, DeliveryID: id, Event: rule.event, RuleID: rule.id, RuleName: rule.name, DedupKey: dedupKey},
		ProjectID:        *rule.project,
		Day:              window.dayStart.Format(time.DateOnly),
		Start:            window.dayStart.Format(time.RFC3339),
		End:              window.dayEnd.Format(time.RFC3339),
		Actual:           days.actual.String(),
		Baseline:         evidence.baseline.String(),
		Increase:         evidence.increase.String(),
		Percent:          evidence.percent.String(),
		DaysUsed:         evidence.daysUsed,
		WindowDays:       c.BaselineDays,
		RequiredPercent:  c.IncreasePercent.String(),
		RequiredIncrease: c.MinimumIncrease.String(),
		Currency:         days.currency,
		UsagePath:        usagePath,
	})
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, insertSignalDeliverySQL, id, rule.id, rule.event, dedupKey, false, payload)
	return int(tag.RowsAffected()), err
}

type spendSpikePayload struct {
	envelope
	ProjectID        string  `json:"project_id"`
	Day              string  `json:"day"`
	Start            string  `json:"start"`
	End              string  `json:"end"`
	Actual           string  `json:"actual_spend"`
	Baseline         string  `json:"baseline_spend"`
	Increase         string  `json:"increase"`
	Percent          string  `json:"increase_percent"`
	DaysUsed         int     `json:"baseline_days_used"`
	WindowDays       int     `json:"baseline_days"`
	RequiredPercent  string  `json:"required_increase_percent"`
	RequiredIncrease string  `json:"required_minimum_increase"`
	Currency         *string `json:"currency"`
	UsagePath        string  `json:"usage_path"`
}

const spikeEvaluatedSQL = `SELECT checked_at FROM olp.notification_signal_states WHERE rule_id=$1 AND subject=$2`

const markSpikeEvaluatedSQL = `INSERT INTO olp.notification_signal_states(rule_id,subject) VALUES($1,$2)
 ON CONFLICT(rule_id,subject) DO UPDATE SET checked_at=now()`

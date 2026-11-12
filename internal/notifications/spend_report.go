package notifications

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage/reports"
)

func (c spendReportConfig) claim(ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) (int, error) {
	var end, start time.Time
	if err := tx.QueryRow(ctx, reportPeriodSQL, c.Period.unit()).Scan(&end, &start); err != nil {
		return 0, err
	}
	if !end.After(rule.createdAt) {
		return 0, nil
	}
	dedupKey := "spend:" + string(c.Period) + ":" + end.UTC().Format(time.RFC3339)
	var queued bool
	if err := tx.QueryRow(ctx, reportAlreadyQueuedSQL, rule.id, dedupKey).Scan(&queued); err != nil {
		return 0, err
	}
	if queued {
		return 0, nil
	}
	filters := reports.Filters{Start: start, End: end, Projects: projectScope(rule.project)}
	summary, err := reports.ReadSummary(ctx, tx, filters, env.workerClock())
	if err != nil {
		return 0, err
	}
	breakdowns := make(map[string]reports.Breakdown, 3)
	for _, dimension := range []reports.Dimension{reports.DimensionProject, reports.DimensionRoute, reports.DimensionAPIKey} {
		breakdown, err := reports.ReadBreakdown(ctx, tx, filters, dimension, 10)
		if err != nil {
			return 0, err
		}
		breakdowns[string(dimension)] = breakdown
	}
	id := env.newID()
	payload, err := json.Marshal(spendReportPayload{
		envelope: envelope{Version: 1, DeliveryID: id, Event: rule.event, RuleID: rule.id, RuleName: rule.name, DedupKey: dedupKey},
		Period:   string(c.Period), Start: start.UTC().Format(time.RFC3339), End: end.UTC().Format(time.RFC3339),
		Summary: summary, Breakdowns: breakdowns,
	})
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, insertSignalDeliverySQL, id, rule.id, rule.event, dedupKey, false, payload)
	return int(tag.RowsAffected()), err
}

func projectScope(project *string) reports.ProjectScope {
	if project == nil {
		return reports.AllProjects()
	}
	return reports.OnlyProjects(*project)
}

type spendReportPayload struct {
	envelope
	Period     string                       `json:"period"`
	Start      string                       `json:"start"`
	End        string                       `json:"end"`
	Summary    reports.Summary              `json:"summary"`
	Breakdowns map[string]reports.Breakdown `json:"breakdowns"`
}

const reportPeriodSQL = `SELECT date_trunc($1,now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS end_at,
 (date_trunc($1,now() AT TIME ZONE 'UTC')-CASE $1 WHEN 'day' THEN interval '1 day' WHEN 'week' THEN interval '1 week' ELSE interval '1 month' END) AT TIME ZONE 'UTC' AS start_at`

const reportAlreadyQueuedSQL = `SELECT EXISTS(SELECT 1 FROM olp.notification_deliveries WHERE rule_id=$1 AND dedup_key=$2 AND NOT resolved)`

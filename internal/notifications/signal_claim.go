package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/usage/reports"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type signalRule struct {
	id, name  string
	event     contract.NotificationEvent
	project   *string
	createdAt time.Time
}

type notificationSignal struct {
	subject  string
	reading  reading
	evidence map[string]any
}

type reading interface{ isReading() }

type unknown struct{}

type observed struct{ active bool }

type measured struct {
	value decimal.Decimal
	band  band
}

func (unknown) isReading()  {}
func (observed) isReading() {}
func (measured) isReading() {}

func observation(known, active bool) reading {
	if !known {
		return unknown{}
	}
	return observed{active: active}
}

func (w *deliveryWorker) claimSignal(ctx context.Context, tx pgx.Tx, rule signalRule, source signalSource, cooldown time.Duration, signal notificationSignal, now time.Time) (int, error) {
	if _, skip := signal.reading.(unknown); skip {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, createSignalStateSQL, rule.id, signal.subject); err != nil {
		return 0, err
	}
	var current signalState
	if err := tx.QueryRow(ctx, lockSignalStateSQL, rule.id, signal.subject).Scan(&current.active, &current.incident, &current.lastTriggerAt); err != nil {
		return 0, err
	}
	next, err := transitionSignal(current, signal.reading, source.announce, now, cooldown)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, updateSignalStateSQL, rule.id, signal.subject, next.state.active, next.state.incident, next.state.lastTriggerAt); err != nil {
		return 0, err
	}
	if !source.announce.announces(next.change) {
		return 0, nil
	}
	digest := sha256.Sum256([]byte(rule.id + ":" + signal.subject))
	incidentKey := "olp:" + hex.EncodeToString(digest[:]) + ":" + strconv.FormatInt(next.state.incident, 10)
	dedupKey := next.change.dedupKey(incidentKey, now)
	event := source.announce.event(rule.event, next.change)
	resolved := next.change == recovered
	id := w.NewID()
	payload, err := json.Marshal(signalPayload{
		envelope: envelope{
			Version: 1, DeliveryID: id, Event: event, RuleID: rule.id, RuleName: rule.name,
			DedupKey: dedupKey, Resolved: resolved,
		},
		IncidentKey: incidentKey, Subject: signal.subject, Incident: next.state.incident,
		OccurredAt: now.UTC().Format(time.RFC3339Nano), Evidence: signal.evidence,
	})
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, insertSignalDeliverySQL, id, rule.id, event, dedupKey, resolved, payload)
	return int(tag.RowsAffected()), err
}

func (c signalChange) dedupKey(incidentKey string, now time.Time) string {
	if c == recovered {
		return incidentKey
	}
	return incidentKey + ":" + strconv.FormatInt(now.UnixNano(), 10)
}

func (c spendReportConfig) claim(w *deliveryWorker, ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) (int, error) {
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
	summary, err := reports.ReadSummary(ctx, tx, filters, w.Now())
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
	id := w.NewID()
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

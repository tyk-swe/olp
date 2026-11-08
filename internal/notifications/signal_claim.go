package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tyk-swe/olp/internal/usage"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type signalRule struct {
	id, name      string
	event         contract.NotificationEvent
	project       *string
	configuration ruleConfiguration
	createdAt     time.Time
}

type notificationSignal struct {
	subject     string
	known       bool
	active      bool
	measurement *string
	evidence    map[string]any
}

func (w *deliveryWorker) claimSignal(ctx context.Context, tx pgx.Tx, rule signalRule, source signalSource, signal notificationSignal, now time.Time) (int, error) {
	if !signal.known {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, createSignalStateSQL, rule.id, signal.subject); err != nil {
		return 0, err
	}
	var active bool
	var incident int64
	var lastTriggerAt *time.Time
	if err := tx.QueryRow(ctx, lockSignalStateSQL, rule.id, signal.subject).Scan(&active, &incident, &lastTriggerAt); err != nil {
		return 0, err
	}
	desired := signal.active
	if signal.measurement != nil {
		var err error
		desired, err = rule.configuration.breached(*signal.measurement, active)
		if err != nil {
			return 0, err
		}
	}
	next, err := transitionSignal(active, incident, lastTriggerAt, desired, now, time.Duration(rule.configuration.CooldownSeconds)*time.Second)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, updateSignalStateSQL, rule.id, signal.subject, next.active, next.incident, next.lastTriggerAt); err != nil {
		return 0, err
	}
	if !next.recover && (!next.trigger || source.recoveryOnly) {
		return 0, nil
	}
	digest := sha256.Sum256([]byte(rule.id + ":" + signal.subject))
	incidentKey := "olp:" + hex.EncodeToString(digest[:]) + ":" + strconv.FormatInt(next.incident, 10)
	dedupKey := incidentKey
	event := rule.event
	if next.recover && source.recoveredAs != "" {
		event = source.recoveredAs
	}
	if !next.recover {
		dedupKey += ":" + strconv.FormatInt(now.UnixNano(), 10)
	}
	id := w.NewID()
	payload, err := json.Marshal(signalPayload{
		Version: 1, DeliveryID: id, Event: event, RuleID: rule.id, RuleName: rule.name,
		incidentRef: incidentRef{Key: incidentKey, Subject: signal.subject, Number: &next.incident},
		DedupKey:    dedupKey, Resolved: next.recover, OccurredAt: now.UTC().Format(time.RFC3339Nano),
		Evidence: signal.evidence,
	})
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, insertSignalDeliverySQL, id, rule.id, event, dedupKey, next.recover, payload)
	return int(tag.RowsAffected()), err
}

func claimSpendReport(w *deliveryWorker, ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) (int, error) {
	unit := map[string]string{"daily": "day", "weekly": "week", "monthly": "month"}[rule.configuration.Period]
	if unit == "" {
		return 0, fmt.Errorf("invalid spend report period %q", rule.configuration.Period)
	}
	var end, start time.Time
	if err := tx.QueryRow(ctx, reportPeriodSQL, unit).Scan(&end, &start); err != nil {
		return 0, err
	}
	if !end.After(rule.createdAt) {
		return 0, nil
	}
	dedupKey := "spend:" + rule.configuration.Period + ":" + end.UTC().Format(time.RFC3339)
	var queued bool
	if err := tx.QueryRow(ctx, reportAlreadyQueuedSQL, rule.id, dedupKey).Scan(&queued); err != nil {
		return 0, err
	}
	if queued {
		return 0, nil
	}
	filters := usage.Filters{Start: start, End: end, AllProjects: rule.project == nil}
	if rule.project != nil {
		filters.AllowedProjects = []string{*rule.project}
	}
	summary, err := usage.ReadSummary(ctx, tx, filters, w.Now())
	if err != nil {
		return 0, err
	}
	breakdowns := make(map[string]usage.Breakdown, 3)
	for _, dimension := range []string{usage.DimensionProject, usage.DimensionRoute, usage.DimensionAPIKey} {
		breakdown, err := usage.ReadBreakdown(ctx, tx, filters, dimension, 10)
		if err != nil {
			return 0, err
		}
		breakdowns[dimension] = breakdown
	}
	id := w.NewID()
	payload, err := json.Marshal(spendReportPayload{
		Version: 1, DeliveryID: id, Event: rule.event, RuleID: rule.id, RuleName: rule.name,
		DedupKey: dedupKey, Period: rule.configuration.Period,
		Start: start.UTC().Format(time.RFC3339), End: end.UTC().Format(time.RFC3339),
		Summary: summary, Breakdowns: breakdowns,
	})
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, insertSignalDeliverySQL, id, rule.id, rule.event, dedupKey, false, payload)
	return int(tag.RowsAffected()), err
}

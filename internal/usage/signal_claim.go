package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/notifications"
)

type signalRule struct {
	id, event, name string
	project         *string
	configuration   notifications.RuleConfiguration
	createdAt       time.Time
}

type notificationSignal struct {
	subject     string
	known       bool
	active      bool
	measurement *string
	evidence    map[string]any
}

func (w *notificationWorker) claimSignal(ctx context.Context, tx pgx.Tx, rule signalRule, signal notificationSignal, now time.Time) (int, error) {
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
		desired, err = rule.configuration.ThresholdState(*signal.measurement, active)
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
	if !next.trigger && !next.recover || rule.event == "provider.circuit.closed" && !next.recover {
		return 0, nil
	}
	digest := sha256.Sum256([]byte(rule.id + ":" + signal.subject))
	incidentKey := "olp:" + hex.EncodeToString(digest[:]) + ":" + strconv.FormatInt(next.incident, 10)
	dedupKey := incidentKey
	event := rule.event
	if next.recover && event == "provider.circuit.open" {
		event = "provider.circuit.closed"
	}
	if !next.recover {
		dedupKey += ":" + strconv.FormatInt(now.UnixNano(), 10)
	}
	id := w.newID()
	payload, err := json.Marshal(map[string]any{
		"version": 1, "delivery_id": id, "event": event, "rule_id": rule.id, "rule_name": rule.name,
		"subject": signal.subject, "incident": next.incident, "incident_key": incidentKey,
		"dedup_key": dedupKey, "resolved": next.recover, "occurred_at": now.UTC().Format(time.RFC3339Nano),
		"evidence": signal.evidence,
	})
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, insertSignalDeliverySQL, id, rule.id, event, dedupKey, next.recover, payload)
	return int(tag.RowsAffected()), err
}

func (w *notificationWorker) claimSpendReport(ctx context.Context, tx pgx.Tx, rule signalRule) (int, error) {
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
	filters := Filters{Start: start, End: end, AllProjects: rule.project == nil}
	if rule.project != nil {
		filters.AllowedProjects = []string{*rule.project}
	}
	summary, err := ReadSummary(ctx, tx, filters, w.now())
	if err != nil {
		return 0, err
	}
	breakdowns := make(map[string]Breakdown, 3)
	for _, dimension := range []string{DimensionProject, DimensionRoute, DimensionAPIKey} {
		breakdown, err := ReadBreakdown(ctx, tx, filters, dimension, 10)
		if err != nil {
			return 0, err
		}
		breakdowns[dimension] = breakdown
	}
	id := w.newID()
	payload, err := json.Marshal(map[string]any{
		"version": 1, "delivery_id": id, "event": rule.event, "rule_id": rule.id, "rule_name": rule.name,
		"dedup_key": dedupKey, "resolved": false, "period": rule.configuration.Period,
		"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339),
		"summary": summary, "breakdowns": breakdowns,
	})
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, insertSignalDeliverySQL, id, rule.id, rule.event, dedupKey, false, payload)
	return int(tag.RowsAffected()), err
}

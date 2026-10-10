package usage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/notifications"
)

func (w *notificationWorker) claimSignals(ctx context.Context, tx pgx.Tx, limiter *limits.Limiter, reference *catalog.Signed) (int, error) {
	rows, err := tx.Query(ctx, signalRulesSQL)
	if err != nil {
		return 0, err
	}
	rules := []signalRule{}
	for rows.Next() {
		if len(rules) == 1000 {
			rows.Close()
			return 0, errSignalLimit
		}
		var rule signalRule
		var raw json.RawMessage
		if err := rows.Scan(&rule.id, &rule.event, &rule.name, &rule.project, &raw, &rule.createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		rule.configuration, err = notifications.ParseRuleConfiguration(rule.event, raw)
		if err != nil {
			rows.Close()
			return 0, err
		}
		rules = append(rules, rule)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(rules) == 0 {
		return 0, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, signalClockSQL).Scan(&now); err != nil {
		return 0, err
	}
	claimed := 0
	for _, rule := range rules {
		if rule.event == "report.spend" {
			count, err := w.claimSpendReport(ctx, tx, rule)
			if err != nil {
				return 0, err
			}
			claimed += count
			continue
		}
		signals, err := readRuleSignals(ctx, tx, rule, now, limiter, reference)
		if errors.Is(err, errSignalUnavailable) {
			w.log.Warn("notification signal evaluation unavailable", "event", rule.event, "code", "signal_source")
			continue
		}
		if err != nil {
			return 0, err
		}
		seen := make(map[string]bool, len(signals))
		for _, signal := range signals {
			seen[signal.subject] = true
			count, err := w.claimSignal(ctx, tx, rule, signal, now)
			if err != nil {
				return 0, err
			}
			claimed += count
		}
		count, err := w.recoverMissingSignals(ctx, tx, rule, seen, now)
		if err != nil {
			return 0, err
		}
		claimed += count
	}
	return claimed, nil
}

func (w *notificationWorker) recoverMissingSignals(ctx context.Context, tx pgx.Tx, rule signalRule, seen map[string]bool, now time.Time) (int, error) {
	switch rule.event {
	case "runtime.install_failed", "worker.stale", "provider.credential.failing":
		return 0, nil
	}
	rows, err := tx.Query(ctx, activeSignalStatesSQL, rule.id)
	if err != nil {
		return 0, err
	}
	subjects := []string{}
	for rows.Next() {
		if len(subjects) == 10000 {
			rows.Close()
			return 0, errSignalLimit
		}
		var subject string
		var incident int64
		var triggered *time.Time
		if err := rows.Scan(&subject, &incident, &triggered); err != nil {
			rows.Close()
			return 0, err
		}
		subjects = append(subjects, subject)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	claimed := 0
	for _, subject := range subjects {
		if seen[subject] {
			continue
		}
		count, err := w.claimSignal(ctx, tx, rule, notificationSignal{
			subject: subject, known: true,
			evidence: map[string]any{"recovery_reason": "subject_no_longer_in_scope"},
		}, now)
		if err != nil {
			return 0, err
		}
		claimed += count
	}
	return claimed, nil
}

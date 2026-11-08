package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const evaluatedRuleLimit = 1000

type signalEnv struct {
	now       time.Time
	limiter   *limits.Limiter
	reference *catalog.Signed
}

type evaluator func(w *deliveryWorker, ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) (int, error)

type signalReader func(ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) ([]notificationSignal, error)

type signalSource struct {
	read            signalReader
	recoversMissing bool
	recoveredAs     contract.NotificationEvent
	recoveryOnly    bool
}

func (w *deliveryWorker) claimSignals(ctx context.Context, tx pgx.Tx) (int, error) {
	rules, err := readEvaluatedRules(ctx, tx)
	if err != nil || len(rules) == 0 {
		return 0, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, signalClockSQL).Scan(&now); err != nil {
		return 0, err
	}
	env := signalEnv{now: now, limiter: w.Limiter, reference: w.Catalog}
	claimed := 0
	for _, rule := range rules {
		count, err := events[rule.event].evaluate(w, ctx, tx, rule, env)
		if err != nil {
			return 0, err
		}
		claimed += count
	}
	return claimed, nil
}

func readEvaluatedRules(ctx context.Context, tx pgx.Tx) ([]signalRule, error) {
	rows, err := tx.Query(ctx, signalRulesSQL, evaluatedEvents, evaluatedRuleLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []signalRule{}
	for rows.Next() {
		if len(rules) == evaluatedRuleLimit {
			return nil, errSignalLimit
		}
		var rule signalRule
		var raw json.RawMessage
		if err := rows.Scan(&rule.id, &rule.event, &rule.name, &rule.project, &raw, &rule.createdAt); err != nil {
			return nil, err
		}
		if rule.configuration, err = events[rule.event].configuration.parse(raw); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func signals(source signalSource) evaluator {
	return func(w *deliveryWorker, ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) (int, error) {
		readings, err := source.read(ctx, tx, rule, env)
		if errors.Is(err, errSignalUnavailable) {
			w.Log.Warn("notification signal evaluation unavailable", "event", rule.event, "code", "signal_source")
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		seen := make(map[string]bool, len(readings))
		claimed := 0
		for _, signal := range readings {
			seen[signal.subject] = true
			count, err := w.claimSignal(ctx, tx, rule, source, signal, env.now)
			if err != nil {
				return 0, err
			}
			claimed += count
		}
		if !source.recoversMissing {
			return claimed, nil
		}
		recovered, err := w.recoverMissingSignals(ctx, tx, rule, source, seen, env.now)
		return claimed + recovered, err
	}
}

func (w *deliveryWorker) recoverMissingSignals(ctx context.Context, tx pgx.Tx, rule signalRule, source signalSource, seen map[string]bool, now time.Time) (int, error) {
	subjects, err := activeSignalSubjects(ctx, tx, rule.id)
	if err != nil {
		return 0, err
	}
	claimed := 0
	for _, subject := range subjects {
		if seen[subject] {
			continue
		}
		count, err := w.claimSignal(ctx, tx, rule, source, notificationSignal{
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

func activeSignalSubjects(ctx context.Context, tx pgx.Tx, ruleID string) ([]string, error) {
	rows, err := tx.Query(ctx, activeSignalStatesSQL, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	subjects := []string{}
	for rows.Next() {
		if len(subjects) == signalScopeLimit {
			return nil, errSignalLimit
		}
		var subject string
		var incident int64
		var triggered *time.Time
		if err := rows.Scan(&subject, &incident, &triggered); err != nil {
			return nil, err
		}
		subjects = append(subjects, subject)
	}
	return subjects, rows.Err()
}

type signalTransition struct {
	active        bool
	incident      int64
	lastTriggerAt *time.Time
	trigger       bool
	recover       bool
}

func transitionSignal(active bool, incident int64, lastTriggerAt *time.Time, desired bool, now time.Time, cooldown time.Duration) (signalTransition, error) {
	if desired && !active && incident == math.MaxInt64 {
		return signalTransition{}, errors.New("notification incident counter exhausted")
	}
	next := signalTransition{active: desired, incident: incident, lastTriggerAt: lastTriggerAt, recover: active && !desired}
	if desired && !active {
		next.incident++
	}
	if desired && (lastTriggerAt == nil || !now.Before(lastTriggerAt.Add(cooldown))) {
		next.trigger = true
		next.lastTriggerAt = &now
	}
	return next, nil
}

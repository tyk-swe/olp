package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const evaluatedRuleLimit = 1000

type signalEnv struct {
	now       time.Time
	fleet     *fleet.State
	reference *catalog.Signed
}

type evaluator func(w *deliveryWorker, ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) (int, error)

type signalReader func(ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) ([]notificationSignal, error)

type signalSource struct {
	read            signalReader
	recoversMissing bool
	announce        announce
}

type announce interface {
	announces(signalChange) bool
	event(rule contract.NotificationEvent, change signalChange) contract.NotificationEvent
}

type triggersAndRecoveries struct{}

type recoveriesAs struct{ recovery contract.NotificationEvent }

type recoveriesOnly struct{}

func (triggersAndRecoveries) announces(change signalChange) bool { return change != quiet }

func (triggersAndRecoveries) event(rule contract.NotificationEvent, _ signalChange) contract.NotificationEvent {
	return rule
}

func (recoveriesAs) announces(change signalChange) bool { return change != quiet }

func (a recoveriesAs) event(rule contract.NotificationEvent, change signalChange) contract.NotificationEvent {
	if change == recovered {
		return a.recovery
	}
	return rule
}

func (recoveriesOnly) announces(change signalChange) bool { return change == recovered }

func (recoveriesOnly) event(rule contract.NotificationEvent, _ signalChange) contract.NotificationEvent {
	return rule
}

type evaluatedRule struct {
	rule     signalRule
	evaluate evaluator
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
	env := signalEnv{now: now, fleet: w.Fleet, reference: w.Catalog}
	claimed := 0
	for _, rule := range rules {
		count, err := rule.evaluate(w, ctx, tx, rule.rule, env)
		if err != nil {
			return 0, err
		}
		claimed += count
	}
	return claimed, nil
}

func readEvaluatedRules(ctx context.Context, tx pgx.Tx) ([]evaluatedRule, error) {
	rows, err := tx.Query(ctx, signalRulesSQL, evaluatedEvents, evaluatedRuleLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []evaluatedRule{}
	for rows.Next() {
		if len(rules) == evaluatedRuleLimit {
			return nil, errSignalLimit
		}
		var rule signalRule
		var raw json.RawMessage
		if err := rows.Scan(&rule.id, &rule.event, &rule.name, &rule.project, &raw, &rule.createdAt); err != nil {
			return nil, err
		}
		configuration, err := events[rule.event].configure(raw)
		if err != nil {
			return nil, err
		}
		rules = append(rules, evaluatedRule{rule: rule, evaluate: configuration.evaluate})
	}
	return rules, rows.Err()
}

func signals(source signalSource, cooldownSeconds int) evaluator {
	cooldown := time.Duration(cooldownSeconds) * time.Second
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
			count, err := w.claimSignal(ctx, tx, rule, source, cooldown, signal, env.now)
			if err != nil {
				return 0, err
			}
			claimed += count
		}
		if !source.recoversMissing {
			return claimed, nil
		}
		recovered, err := w.recoverMissingSignals(ctx, tx, rule, source, cooldown, seen, env.now)
		return claimed + recovered, err
	}
}

func (w *deliveryWorker) recoverMissingSignals(ctx context.Context, tx pgx.Tx, rule signalRule, source signalSource, cooldown time.Duration, seen map[string]bool, now time.Time) (int, error) {
	subjects, err := activeSignalSubjects(ctx, tx, rule.id)
	if err != nil {
		return 0, err
	}
	claimed := 0
	for _, subject := range subjects {
		if seen[subject] {
			continue
		}
		count, err := w.claimSignal(ctx, tx, rule, source, cooldown, notificationSignal{
			subject: subject, reading: observed{active: false},
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

type signalState struct {
	active        bool
	incident      int64
	lastTriggerAt *time.Time
}

type signalChange int

const (
	quiet signalChange = iota
	triggered
	recovered
)

type signalTransition struct {
	state  signalState
	change signalChange
}

func transitionSignal(current signalState, r reading, a announce, now time.Time, cooldown time.Duration) (signalTransition, error) {
	paced := pacing(a, cooldown)
	switch r := r.(type) {
	case observed:
		return current.toward(r.active, now, paced)
	case measured:
		return current.toward(r.band.breached(r.value, current.active), now, paced)
	default:
		return signalTransition{state: current}, nil
	}
}

func pacing(a announce, cooldown time.Duration) time.Duration {
	if a.announces(triggered) {
		return cooldown
	}
	return 0
}

func (s signalState) toward(breaching bool, now time.Time, cooldown time.Duration) (signalTransition, error) {
	switch {
	case !breaching && s.active:
		return signalTransition{state: signalState{incident: s.incident, lastTriggerAt: s.lastTriggerAt}, change: recovered}, nil
	case !breaching, s.lastTriggerAt != nil && now.Before(s.lastTriggerAt.Add(cooldown)):
		return signalTransition{state: s}, nil
	case !s.active && s.incident == math.MaxInt64:
		return signalTransition{}, errors.New("notification incident counter exhausted")
	default:
		return signalTransition{state: signalState{active: true, incident: s.breachIncident(), lastTriggerAt: &now}, change: triggered}, nil
	}
}

func (s signalState) breachIncident() int64 {
	if s.active {
		return s.incident
	}
	return s.incident + 1
}

package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

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
	rows, err := tx.Query(ctx, activeSignalStatesSQL, ruleID, signalScopeLimit+1)
	if err != nil {
		return nil, err
	}
	subjects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if len(subjects) > signalScopeLimit {
		return nil, errSignalLimit
	}
	return subjects, nil
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

func (unknown) isReading() {}

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

type signalPayload struct {
	envelope
	IncidentKey string         `json:"incident_key"`
	Subject     string         `json:"subject,omitempty"`
	Incident    int64          `json:"incident"`
	OccurredAt  string         `json:"occurred_at"`
	Evidence    map[string]any `json:"evidence"`
}

const signalRulesSQL = `SELECT r.id::text,r.event,r.name,r.project_id::text,r.configuration,r.created_at
 FROM olp.notification_rules r JOIN olp.notification_destinations d ON d.id=r.destination_id
 WHERE r.enabled AND d.enabled AND r.event=ANY($1)
 ORDER BY r.id LIMIT $2`

const signalClockSQL = `SELECT now()`

const createSignalStateSQL = `INSERT INTO olp.notification_signal_states(rule_id,subject) VALUES($1,$2) ON CONFLICT DO NOTHING`

const lockSignalStateSQL = `SELECT active,incident,last_trigger_at FROM olp.notification_signal_states WHERE rule_id=$1 AND subject=$2 FOR UPDATE`

const updateSignalStateSQL = `UPDATE olp.notification_signal_states SET active=$3,incident=$4,last_trigger_at=$5,checked_at=now() WHERE rule_id=$1 AND subject=$2`

const insertSignalDeliverySQL = `INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
 VALUES($1,$2,$3,$4,$5,$6,'pending') ON CONFLICT DO NOTHING`

const activeSignalStatesSQL = `SELECT subject FROM olp.notification_signal_states WHERE rule_id=$1 AND active ORDER BY subject LIMIT $2 FOR UPDATE`

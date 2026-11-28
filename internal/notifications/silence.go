package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type silenceWindow struct {
	startsAt, endsAt *time.Time
}

func (w silenceWindow) active(now time.Time) bool {
	return w.startsAt != nil && w.endsAt != nil && !now.Before(*w.startsAt) && now.Before(*w.endsAt)
}

func (r signalRule) transition(current signalState, reading reading, policy announce, env signalEnv, cooldown time.Duration) (signalTransition, error) {
	if r.event != contract.NotificationEventRouteLatency {
		return transitionSignal(current, reading, policy, env.now, cooldown)
	}
	muted := r.silence.active(env.silenceNow)
	next, err := transitionSignal(current, reading, policy, env.now, latencyCooldown(current, muted, cooldown))
	if err != nil {
		return signalTransition{}, err
	}
	switch {
	case next.change == triggered && muted:
		return signalTransition{state: signalState{
			active: true, incident: next.state.incident, lastTriggerAt: current.lastTriggerAt,
			triggerPending: current.triggerPending || !current.active,
		}}, nil
	case next.change == recovered && current.triggerPending:
		return signalTransition{state: next.state}, nil
	default:
		return next, nil
	}
}

func latencyCooldown(current signalState, muted bool, cooldown time.Duration) time.Duration {
	if muted || current.triggerPending {
		return 0
	}
	return cooldown
}

func parseSilence(ctx context.Context, tx pgx.Tx, target ruleTarget, raw json.RawMessage) (silenceWindow, error) {
	if raw == nil {
		return silenceWindow{}, nil
	}
	if target.event != contract.NotificationEventRouteLatency || target.projectID == nil {
		return silenceWindow{}, problem.Invalid("silence", "Only project-scoped route latency webhook rules support silences.")
	}
	if string(raw) == "null" {
		return silenceWindow{}, nil
	}
	var input struct {
		StartsAt time.Time `json:"starts_at"`
		EndsAt   time.Time `json:"ends_at"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.StartsAt.IsZero() || input.EndsAt.IsZero() {
		return silenceWindow{}, problem.Invalid("silence", "Send both start and end as RFC 3339 timestamps.")
	}
	start, end := input.StartsAt.UTC().Truncate(time.Microsecond), input.EndsAt.UTC().Truncate(time.Microsecond)
	var channel contract.NotificationDestinationType
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT type, now() FROM olp.notification_destinations WHERE id=$1", target.destinationID).Scan(&channel, &now); err != nil {
		return silenceWindow{}, err
	}
	if channel != contract.NotificationDestinationTypeWebhook {
		return silenceWindow{}, problem.Invalid("silence", "Only webhook destinations support silences.")
	}
	if !end.After(start) || !end.After(now) || start.Year() < 1 || end.Year() > 9999 {
		return silenceWindow{}, problem.Invalid("silence", "End must be after the start and in the future.")
	}
	return silenceWindow{startsAt: &start, endsAt: &end}, nil
}

package resources

import (
	"context"
	"slices"

	"github.com/tyk-swe/olp/internal/codemode"
)

func allowanceWindows(a codemode.Allowance) []codemode.AllowanceWindow {
	if len(a.Windows) == 0 && a.RemainingPercent != nil {
		return []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 100 - *a.RemainingPercent, RemainingPercent: *a.RemainingPercent, ResetsAt: a.ResetsAt, ObservedAt: a.ObservedAt}}
	}
	return a.Windows
}

func mergeCodeAllowance(current, incoming codemode.Allowance) codemode.Allowance {
	result := current
	if incoming.ObservedAt.After(current.ObservedAt) {
		result.ObservedAt = incoming.ObservedAt
	}
	result.RemainingTokens, result.TokenObservation = mergeCodeCount(current.RemainingTokens, current.TokenObservation, current, incoming.RemainingTokens, incoming.TokenObservation, incoming)
	result.RemainingRequests, result.RequestObservation = mergeCodeCount(current.RemainingRequests, current.RequestObservation, current, incoming.RemainingRequests, incoming.RequestObservation, incoming)
	result.Windows = slices.Clone(allowanceWindows(current))
	for _, w := range allowanceWindows(incoming) {
		index := slices.IndexFunc(result.Windows, func(old codemode.AllowanceWindow) bool {
			return old.LimitID == w.LimitID && old.Window == w.Window
		})
		if index < 0 {
			result.Windows = append(result.Windows, w)
		} else if w.ObservedAt.After(result.Windows[index].ObservedAt) {
			result.Windows[index] = w
		}
	}
	result.Normalize()
	if incoming.Credits != nil && (current.Credits == nil || incoming.Credits.ObservedAt.After(current.Credits.ObservedAt)) {
		result.Credits = incoming.Credits
	}
	return result
}

func mergeCodeCount(value *int64, observation *codemode.CountObservation, current codemode.Allowance, incoming *int64, incomingObservation *codemode.CountObservation, update codemode.Allowance) (*int64, *codemode.CountObservation) {
	if value != nil && observation == nil {
		observation = &codemode.CountObservation{ResetsAt: current.ResetsAt, ObservedAt: current.ObservedAt}
	}
	if incoming == nil {
		return value, observation
	}
	if incomingObservation == nil {
		incomingObservation = &codemode.CountObservation{ResetsAt: update.ResetsAt, ObservedAt: update.ObservedAt}
	}
	if observation == nil || incomingObservation.ObservedAt.After(observation.ObservedAt) {
		return incoming, incomingObservation
	}
	return value, observation
}

func (s *CodeStore) ObserveOutcome(ctx context.Context, attemptID string, outcome codemode.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE olp.code_attempts SET
		upstream_status=coalesce($2,upstream_status),outcome_origin=$3,outcome=$4,outcome_observed_at=$5
		WHERE id=$1 AND (outcome_observed_at IS NULL OR outcome_observed_at<=$5)
		AND (outcome IS NULL OR outcome='headers' OR
		  (outcome IN ('interrupted','canceled','transport_error') AND $3='upstream' AND $4<>'headers'))
		AND (state<>'prepared' OR $3<>'upstream')`,
		attemptID, outcome.UpstreamStatus, outcome.Origin, outcome.Kind, outcome.ObservedAt)
	return err
}

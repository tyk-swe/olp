package resources

import (
	"context"
	"slices"
	"strings"

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
		if incoming.RemainingTokens != nil || incoming.RemainingRequests != nil {
			result.RemainingTokens, result.RemainingRequests = incoming.RemainingTokens, incoming.RemainingRequests
			result.ResetsAt = incoming.ResetsAt
		}
	}
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
	slices.SortFunc(result.Windows, func(a, b codemode.AllowanceWindow) int {
		return strings.Compare(a.LimitID+":"+a.Window, b.LimitID+":"+b.Window)
	})
	for _, w := range result.Windows {
		if w.LimitID == "codex" && w.Window == "primary" {
			remaining := w.RemainingPercent
			result.RemainingPercent, result.ResetsAt = &remaining, w.ResetsAt
		}
	}
	if incoming.Credits != nil && (current.Credits == nil || incoming.Credits.ObservedAt.After(current.Credits.ObservedAt)) {
		result.Credits = incoming.Credits
	}
	return result
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

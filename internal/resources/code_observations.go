package resources

import (
	"context"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/codemode"
)

func (s *CodeStore) ObserveAllowanceCharge(ctx context.Context, attemptID string, charge json.RawMessage) error {
	if err := codemode.ValidateAllowanceCharge(charge); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE olp.code_attempts SET allowance_charge=$2 WHERE id=$1 AND paid_binding_id IS NULL`, attemptID, []byte(charge))
	return err
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

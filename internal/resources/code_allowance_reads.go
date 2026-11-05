package resources

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routing"
)

const freshActiveWindow = `EXISTS(SELECT 1
	FROM jsonb_array_elements(CASE WHEN jsonb_typeof(a.allowance->'windows')='array' THEN a.allowance->'windows' ELSE '[]'::jsonb END) w
	WHERE w->>'evidence'='active'
	AND CASE WHEN w->>'observed_at' ~ '^\d{4}-\d{2}-\d{2}T' THEN (w->>'observed_at')::timestamptz > $1::timestamptz ELSE false END)`

func (s *CodeStore) DueAllowanceReads(ctx context.Context, now time.Time, limit int, qualifiedProfileIDs []string) ([]codeallowance.DueAccount, error) {
	freshSince := now.Add(-codeallowance.ReadFreshFor)
	rows, err := s.Pool.Query(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.name,a.enabled,a.models,a.etag::text,a.health,a.health_observed_at,a.unavailable_until,a.allowance,a.allowance_read_requested_at,a.allowance_read_at,p.configuration
		FROM olp.code_accounts a JOIN olp.providers p ON p.id=a.provider_id AND p.project_id=a.project_id
		WHERE a.enabled AND p.state<>'disabled' AND p.configuration->>'kind'='plugin' AND p.configuration->>'auth_mode'='grant' AND p.configuration->>'profile_id'=ANY($3::text[])
		AND ((a.allowance_read_requested_at IS NOT NULL AND (a.allowance_read_at IS NULL OR a.allowance_read_requested_at>a.allowance_read_at))
			OR ((a.allowance_read_at IS NULL OR a.allowance_read_at<=$1::timestamptz) AND NOT `+freshActiveWindow+`))
		ORDER BY (a.allowance_read_requested_at IS NULL),a.allowance_read_requested_at,a.allowance_read_at NULLS FIRST,a.id LIMIT $2`, freshSince, limit, qualifiedProfileIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (codeallowance.DueAccount, error) {
		var due codeallowance.DueAccount
		var configuration []byte
		account := &due.Account
		if err := row.Scan(&account.ID, &account.ProjectID, &account.ProviderID, &account.CredentialID, &account.Principal, &account.Name, &account.Enabled, &account.Models, &account.ETag, &account.Health, &account.HealthObservedAt, &account.UnavailableUntil, &account.Allowance, &account.AllowanceReadRequestedAt, &account.AllowanceReadAt, &configuration); err != nil {
			return due, err
		}
		var decoded routing.Configuration
		if err := json.Unmarshal(configuration, &decoded); err != nil {
			return due, err
		}
		due.Configuration = decoded
		return due, nil
	})
}

func (s *CodeStore) CompleteAllowanceRead(ctx context.Context, account string, readAt time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE olp.code_accounts SET allowance_read_at=$2,
		allowance_read_requested_at=CASE WHEN allowance_read_requested_at<=$2 THEN NULL ELSE allowance_read_requested_at END
		WHERE id=$1`, account, readAt)
	return err
}

func (s *CodeStore) InvalidateActiveAllowance(ctx context.Context, account string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var document []byte
	if err := tx.QueryRow(ctx, `SELECT allowance FROM olp.code_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&document); err != nil {
		return err
	}
	if len(document) == 0 {
		return tx.Commit(ctx)
	}
	var current codemode.Allowance
	if err := json.Unmarshal(document, &current); err != nil {
		return err
	}
	activePrimary := slices.ContainsFunc(current.Windows, func(w codemode.AllowanceWindow) bool {
		return w.LimitID == "codex" && w.Window == "primary" && w.EffectiveEvidence() == codemode.AllowanceEvidenceActive
	})
	passivePrimary := slices.IndexFunc(current.Windows, func(w codemode.AllowanceWindow) bool {
		return w.LimitID == "codex" && w.Window == "primary" && w.EffectiveEvidence() == codemode.AllowanceEvidencePassive && w.EffectiveUnit() == codemode.AllowanceUnitPercent
	})
	percent, reset := func() (*float64, *time.Time) {
		if passivePrimary >= 0 {
			return &current.Windows[passivePrimary].RemainingPercent, current.Windows[passivePrimary].ResetsAt
		}
		if activePrimary {
			return nil, nil
		}
		return current.RemainingPercent, current.ResetsAt
	}()
	windows := slices.Collect(func(yield func(codemode.AllowanceWindow) bool) {
		for _, w := range current.Windows {
			if w.EffectiveEvidence() != codemode.AllowanceEvidenceActive && !yield(w) {
				return
			}
		}
	})
	cleared := codemode.Allowance{
		Windows: windows, Credits: current.Credits, RemainingTokens: current.RemainingTokens, RemainingRequests: current.RemainingRequests,
		TokenObservation: current.TokenObservation, RequestObservation: current.RequestObservation,
		RemainingPercent: percent, ResetsAt: reset, ObservedAt: current.ObservedAt,
	}
	encoded, err := json.Marshal(cleared)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, account, encoded); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

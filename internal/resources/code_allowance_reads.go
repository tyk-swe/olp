package resources

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/routing"
)

const freshActiveWindow = `EXISTS(SELECT 1
	FROM jsonb_array_elements(CASE WHEN jsonb_typeof(a.allowance->'windows')='array' THEN a.allowance->'windows' ELSE '[]'::jsonb END) w
	WHERE w->>'evidence'='active'
	AND CASE WHEN w->>'observed_at' ~ '^\d{4}-\d{2}-\d{2}T' THEN (w->>'observed_at')::timestamptz > $1::timestamptz - interval '5 minutes' ELSE false END)`

func (s *CodeStore) DueAllowanceReads(ctx context.Context, now time.Time, limit int) ([]codeallowance.DueAccount, error) {
	rows, err := s.Pool.Query(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.name,a.enabled,a.models,a.etag::text,a.health,a.health_observed_at,a.unavailable_until,a.allowance,a.allowance_read_requested_at,a.allowance_read_at,p.configuration
		FROM olp.code_accounts a JOIN olp.providers p ON p.id=a.provider_id AND p.project_id=a.project_id
		WHERE a.enabled AND p.state<>'disabled' AND `+codeadapter.SQL("p.configuration")+`='codex'
		AND ((a.allowance_read_requested_at IS NOT NULL AND (a.allowance_read_at IS NULL OR a.allowance_read_requested_at>a.allowance_read_at))
			OR ((a.allowance_read_at IS NULL OR a.allowance_read_at<=$1::timestamptz - interval '5 minutes') AND NOT `+freshActiveWindow+`))
		ORDER BY (a.allowance_read_requested_at IS NULL),a.allowance_read_requested_at,a.allowance_read_at NULLS FIRST,a.id LIMIT $2`, now, limit)
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

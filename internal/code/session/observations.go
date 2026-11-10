package session

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/allowance"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/routing"
)

var refusalCodePattern = regexp.MustCompile(`^code_[a-z_]{1,80}$`)

const freshActiveWindow = `EXISTS(SELECT 1
	FROM jsonb_array_elements(CASE WHEN jsonb_typeof(a.allowance->'windows')='array' THEN a.allowance->'windows' ELSE '[]'::jsonb END) w
	WHERE w->>'evidence'='active'
	AND CASE WHEN w->>'observed_at' ~ '^\d{4}-\d{2}-\d{2}T' THEN (w->>'observed_at')::timestamptz > $1::timestamptz ELSE false END)`

func (s *Store) ObserveAllowance(ctx context.Context, account string, incoming code.Allowance) error {
	if err := incoming.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var document []byte
	if err = tx.QueryRow(ctx, `SELECT allowance FROM olp.code_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&document); err != nil {
		return err
	}
	var current code.Allowance
	if len(document) > 0 {
		if err = json.Unmarshal(document, &current); err != nil {
			return err
		}
	}
	merged := current.Merge(incoming)
	if err = merged.Validate(); err != nil {
		return err
	}
	document, err = json.Marshal(merged)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, account, document); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DueAllowanceReads(ctx context.Context, now time.Time, limit int, qualifiedProfileIDs []string) ([]allowance.DueAccount, error) {
	freshSince := now.Add(-allowance.ReadFreshFor)
	rows, err := s.pool.Query(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.name,a.enabled,a.models,a.etag::text,a.health,a.health_observed_at,a.unavailable_until,a.allowance,a.allowance_read_requested_at,a.allowance_read_at,p.configuration
		FROM olp.code_accounts a JOIN olp.providers p ON p.id=a.provider_id AND p.project_id=a.project_id
		WHERE a.enabled AND p.state<>'disabled' AND p.configuration->>'kind'='plugin' AND p.configuration->>'auth_mode'='grant' AND p.configuration->>'profile_id'=ANY($3::text[])
		AND ((a.allowance_read_requested_at IS NOT NULL AND (a.allowance_read_at IS NULL OR a.allowance_read_requested_at>a.allowance_read_at))
			OR ((a.allowance_read_at IS NULL OR a.allowance_read_at<=$1::timestamptz) AND NOT `+freshActiveWindow+`))
		ORDER BY (a.allowance_read_requested_at IS NULL),a.allowance_read_requested_at,a.allowance_read_at NULLS FIRST,a.id LIMIT $2`, freshSince, limit, qualifiedProfileIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (allowance.DueAccount, error) {
		var due allowance.DueAccount
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

func (s *Store) CompleteAllowanceRead(ctx context.Context, account string, readAt time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE olp.code_accounts SET allowance_read_at=$2,
		allowance_read_requested_at=CASE WHEN allowance_read_requested_at<=$2 THEN NULL ELSE allowance_read_requested_at END
		WHERE id=$1`, account, readAt)
	return err
}

func (s *Store) InvalidateActiveAllowance(ctx context.Context, account string) error {
	tx, err := s.pool.Begin(ctx)
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
	var current code.Allowance
	if err := json.Unmarshal(document, &current); err != nil {
		return err
	}
	encoded, err := json.Marshal(current.WithoutActive())
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, account, encoded); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ObserveCooldownClass(ctx context.Context, accountID, model string, classification vendor.Classification, observedAt time.Time) error {
	classes := code.CooldownClasses()
	if !slices.Contains(classes[:], classification.Class) || classification.RetryAt.IsZero() || observedAt.IsZero() {
		return code.Refuse(400, "code_cooldown_invalid")
	}
	if classification.Scope != code.ScopeCredential && classification.Scope != code.ScopeModel {
		return code.Refuse(400, "code_cooldown_invalid")
	}
	if classification.Scope == code.ScopeModel && model == "" {
		return code.Refuse(400, "code_cooldown_invalid")
	}
	var storedModel *string
	if classification.Scope == code.ScopeModel {
		storedModel = &model
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO olp.code_account_cooldowns(account_id,model,class,retry_at,observed_at) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (account_id, (coalesce(model, ''))) DO UPDATE SET
			class=CASE WHEN excluded.retry_at>=code_account_cooldowns.retry_at THEN excluded.class ELSE code_account_cooldowns.class END,
			observed_at=CASE WHEN excluded.retry_at>=code_account_cooldowns.retry_at THEN excluded.observed_at ELSE code_account_cooldowns.observed_at END,
			retry_at=greatest(code_account_cooldowns.retry_at,excluded.retry_at)`,
		accountID, storedModel, string(classification.Class), classification.RetryAt, observedAt); err != nil {
		return err
	}
	if classification.Scope != code.ScopeCredential {
		return nil
	}
	status := code.HealthUnavailable
	if classification.Class == code.CooldownQuota {
		status = code.HealthQuotaLimited
	}
	return s.ObserveHealth(ctx, accountID, code.NewDegradedObservation(status, observedAt, &classification.RetryAt))
}

func (s *Store) ObserveHealth(ctx context.Context, accountID string, observation code.HealthObservation) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE olp.code_accounts SET health=$2,health_observed_at=$3,
		unavailable_until=CASE WHEN $2='healthy' THEN NULL
			ELSE greatest(unavailable_until,coalesce($4::timestamptz,$3::timestamptz+interval '1 minute')) END
		WHERE id=$1 AND (health_observed_at IS NULL OR health_observed_at<$3)
		AND ($2<>'healthy' OR unavailable_until IS NULL OR unavailable_until<=now())`,
		accountID, observation.Status, observation.ObservedAt, observation.RetryAt)
	return err
}

func (s *Store) ObserveAllowanceCharge(ctx context.Context, attemptID string, charge json.RawMessage) error {
	if err := code.ValidateAllowanceCharge(charge); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE olp.code_attempts SET allowance_charge=$2 WHERE id=$1 AND paid_binding_id IS NULL`, attemptID, []byte(charge))
	return err
}

func (s *Store) ObserveOutcome(ctx context.Context, attemptID string, outcome code.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE olp.code_attempts SET
		upstream_status=coalesce($2,upstream_status),outcome_origin=$3,outcome=$4,outcome_observed_at=$5
		WHERE id=$1 AND (outcome_observed_at IS NULL OR outcome_observed_at<=$5)
		AND (outcome IS NULL OR outcome='headers' OR
		  (outcome IN ('interrupted','canceled','transport_error') AND $3='upstream' AND $4<>'headers'))
		AND (state<>'prepared' OR $3<>'upstream')`,
		attemptID, outcome.UpstreamStatus, outcome.Origin, outcome.Kind, outcome.ObservedAt)
	return err
}

func (s *Store) ObserveReference(ctx context.Context, attemptID, externalID string) error {
	if (code.Identity{Conversation: externalID}).Validate() != nil {
		return code.Refuse(400, "code_response_reference_invalid")
	}
	_, err := s.pool.Exec(ctx, `SELECT olp.observe_code_reference($1,$2)`, attemptID, externalID)
	return err
}

func (s *Store) RecordRefusal(ctx context.Context, route code.Route, keyID, digest, refusalCode string) error {
	if digest != "" && !keyauthority.ValidEndUserDigest(digest) {
		return code.Refuse(400, "code_end_user_invalid")
	}
	if !refusalCodePattern.MatchString(refusalCode) {
		return code.Refuse(400, "code_refusal_invalid")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO olp.code_refusals(id,project_id,route_id,api_key_id,code,end_user_digest) SELECT $1,$2,$3,k.id,$5,$6 FROM olp.api_keys k WHERE k.id=$4 AND k.project_id=$2`,
		access.NewID(), route.ProjectID, route.ID, keyID, refusalCode, digest)
	return err
}

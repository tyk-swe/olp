package limits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
)

// ReserveCodeTokens participates in the pin/admission transaction. The caller
// holds the installation row FOR SHARE, excluding budget configuration writes.
func ReserveCodeTokens(ctx context.Context, tx pgx.Tx, attempt codemode.Attempt, bound *codemode.TokenBound) error {
	rows, err := tx.Query(ctx, `SELECT id::text,daily_tokens,monthly_tokens,route_id::text,api_key_id::text FROM olp.code_token_budgets
		WHERE enabled AND project_id=$1 AND (route_id IS NULL OR route_id=$2) AND (api_key_id IS NULL OR api_key_id=$3)
		ORDER BY id FOR UPDATE`, attempt.ProjectID, attempt.RouteID, attempt.APIKeyID)
	if err != nil {
		return err
	}
	type budget struct {
		id         string
		day, month *int64
		route, key *string
	}
	var budgets []budget
	for rows.Next() {
		var b budget
		if err = rows.Scan(&b.id, &b.day, &b.month, &b.route, &b.key); err != nil {
			rows.Close()
			return err
		}
		budgets = append(budgets, b)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(budgets) == 0 {
		return nil
	}
	if err = bound.Validate(); err != nil {
		return err
	}
	for _, b := range budgets {
		for _, window := range []struct {
			period  string
			maximum *int64
		}{{"day", b.day}, {"month", b.month}} {
			if window.maximum == nil {
				continue
			}
			var start time.Time
			if err = tx.QueryRow(ctx, `SELECT date_trunc($1,now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`, window.period).Scan(&start); err != nil {
				return err
			}
			var available bool
			err = tx.QueryRow(ctx, `SELECT COALESCE(sum(CASE WHEN state IN ('prepared','uncertain') THEN reserved_tokens ELSE COALESCE(reported_tokens,0) END),0)<=$6::bigint-$7::bigint
				AND NOT COALESCE(bool_or(state IN ('prepared','uncertain') AND reserved_tokens=0),false)
				AND NOT COALESCE(bool_or(state='bound_violation'),false)
				FROM olp.code_attempts WHERE project_id=$1 AND ($2::uuid IS NULL OR route_id=$2) AND ($3::uuid IS NULL OR api_key_id=$3)
				AND created_at >= $4 AND id<>$5`, attempt.ProjectID, b.route, b.key, start, attempt.ID, *window.maximum, bound.Tokens).Scan(&available)
			if err != nil {
				return err
			}
			if !available {
				return codemode.Refuse(429, "code_token_budget_exhausted")
			}
			if _, err = tx.Exec(ctx, `INSERT INTO olp.code_token_windows(budget_id,period,starts_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, b.id, window.period, start); err != nil {
				return err
			}
			result, err := tx.Exec(ctx, `UPDATE olp.code_token_windows SET reserved=reserved+$4
				WHERE budget_id=$1 AND period=$2 AND starts_at=$3 AND measured+reserved <= $5::bigint-$4::bigint`, b.id, window.period, start, bound.Tokens, *window.maximum)
			if err != nil {
				return err
			}
			if result.RowsAffected() != 1 {
				return codemode.Refuse(429, "code_token_budget_exhausted")
			}
			if _, err = tx.Exec(ctx, `INSERT INTO olp.code_token_reservations(attempt_id,budget_id,period,starts_at,tokens) VALUES($1,$2,$3,$4,$5)`, attempt.ID, b.id, window.period, start, bound.Tokens); err != nil {
				return err
			}
		}
	}
	return nil
}

// SettleCodeTokens keeps missing usage reserved indefinitely. The attempt row
// and all charged windows change in the same transaction, once.
func SettleCodeTokens(ctx context.Context, tx pgx.Tx, id string, usage codemode.Usage, abort bool) error {
	if err := usage.Validate(); err != nil {
		return err
	}
	var state string
	var reported *int64
	var reserved int64
	err := tx.QueryRow(ctx, `SELECT state,reported_tokens,reserved_tokens FROM olp.code_attempts WHERE id=$1 FOR UPDATE`, id).Scan(&state, &reported, &reserved)
	if err != nil {
		return err
	}
	if state == "settled" || state == "bound_violation" || state == "aborted" {
		if abort && state == "aborted" || !abort && usage.Total == nil || reported != nil && usage.Total != nil && *reported == *usage.Total {
			return nil
		}
		return codemode.Refuse(409, "code_settlement_conflict")
	}
	if abort && state != "prepared" {
		return codemode.Refuse(409, "code_dispatch_uncertain")
	}
	if usage.Total == nil && !abort {
		_, err = tx.Exec(ctx, `UPDATE olp.code_attempts SET state='uncertain',input_tokens=COALESCE($2,input_tokens),output_tokens=COALESCE($3,output_tokens),cached_tokens=COALESCE($4,cached_tokens),reasoning_tokens=COALESCE($5,reasoning_tokens),finished_at=now() WHERE id=$1`, id, usage.Input, usage.Output, usage.Cached, usage.Reasoning)
		return err
	}
	actual := int64(0)
	if usage.Total != nil {
		actual = *usage.Total
	}
	rows, err := tx.Query(ctx, `SELECT budget_id::text,period,starts_at,tokens FROM olp.code_token_reservations WHERE attempt_id=$1 ORDER BY budget_id,period`, id)
	if err != nil {
		return err
	}
	type charge struct {
		budget, period string
		start          time.Time
		tokens         int64
	}
	var charges []charge
	for rows.Next() {
		var c charge
		if err = rows.Scan(&c.budget, &c.period, &c.start, &c.tokens); err != nil {
			rows.Close()
			return err
		}
		charges = append(charges, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, c := range charges {
		result, err := tx.Exec(ctx, `UPDATE olp.code_token_windows SET reserved=reserved-$4,measured=measured+$5 WHERE budget_id=$1 AND period=$2 AND starts_at=$3 AND reserved >= $4`, c.budget, c.period, c.start, c.tokens, actual)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return errors.New("code token reservation missing")
		}
	}
	state = "settled"
	if abort {
		state = "aborted"
	} else if reserved > 0 && actual > reserved {
		state = "bound_violation"
	}
	_, err = tx.Exec(ctx, `UPDATE olp.code_attempts SET state=$2,reported_tokens=$3,input_tokens=$4,output_tokens=$5,cached_tokens=$6,reasoning_tokens=$7,finished_at=now() WHERE id=$1`, id, state, usage.Total, usage.Input, usage.Output, usage.Cached, usage.Reasoning)
	if err != nil {
		return fmt.Errorf("code usage settlement: %w", err)
	}
	return nil
}

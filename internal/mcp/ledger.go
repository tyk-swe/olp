package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

type DurableLedger struct {
	Pool  *pgxpool.Pool
	Store *limits.DebitStore
	Auth  *secrets.AuthKey
}

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (l *DurableLedger) currency(ctx context.Context) (string, error) {
	var currency string
	err := l.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')`).Scan(&currency)
	return currency, err
}

func (l *DurableLedger) Reserve(ctx context.Context, in ReserveInput) (ReserveResult, error) {
	if _, ok := parseEvidenceDigest(in.CallDigest); l.Pool == nil || l.Store == nil || l.Auth == nil || in.Authority.ProjectID == nil || !ok {
		return ReserveResult{}, limits.ErrDebitAuthority
	}
	if in.UnitPrice != nil && !routing.ValidToolPrice(*in.UnitPrice) {
		return ReserveResult{}, limits.ErrDebitConflict
	}
	boundaries := in.Authority.ToolDebitBoundaries(time.Hour)
	if !boundaries.Resolved {
		return ReserveResult{}, limits.ErrDebitAuthority
	}
	owners, requests := boundaries.Owners(), boundaries.Requests()
	bound := "0"
	if in.UnitPrice != nil {
		bound = *in.UnitPrice
	}
	ids := make([]string, 5)
	for i := range ids {
		id, err := newID()
		if err != nil {
			return ReserveResult{}, err
		}
		ids[i] = id
	}
	currency, err := l.currency(ctx)
	if err != nil || currency == "" {
		return ReserveResult{}, limits.ErrDebitAuthority
	}
	reservation := &Reservation{
		DispatchID: ids[0], ClaimID: ids[1], RunID: ids[2], StepID: ids[3], AttemptID: ids[4],
		APIKeyID: in.Authority.ID, ProjectID: *in.Authority.ProjectID,
		ToolsetID: in.ToolsetID, ToolsetRevisionID: in.ToolsetRevisionID,
		ToolAlias: in.ToolAlias, InputDigest: in.InputDigest, CallDigest: in.CallDigest,
		UnitPrice: cloneUnitPrice(in.UnitPrice), Currency: currency,
	}
	intent := limits.DebitIntent{
		ID: reservation.DispatchID, APIKeyID: in.Authority.ID, ProjectID: reservation.ProjectID,
		RunID: reservation.RunID, StepID: reservation.StepID, Kind: "tool", ClaimID: reservation.ClaimID,
		Currency: currency, CostBound: bound, ChargeBound: bound, PreparedUntil: time.Now().Add(90 * time.Second),
		Affinity: limits.ExecutionAffinity{ReleaseID: in.ReleaseID, RevisionID: in.ToolsetRevisionID, ToolAlias: in.ToolAlias, InputDigest: in.InputDigest},
	}
	if in.UnitPrice == nil && boundaries.CostBudgeted {
		if err = l.recordRefusedUnpriced(ctx, in, reservation, intent, boundaries); err != nil {
			return ReserveResult{}, err
		}
		return ReserveResult{}, ErrUnpricedToolCall
	}
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return ReserveResult{}, err
	}
	defer tx.Rollback(ctx)
	if err = agents.StartRunTx(ctx, tx, l.Auth, in.Authority, reservation.RunID, "tools", in.ToolsetID, in.ToolsetRevisionID); err != nil {
		return ReserveResult{}, err
	}
	if err = agents.EnsureBoundaryAccountsTx(ctx, tx, in.Authority, boundaries); err != nil {
		return ReserveResult{}, err
	}
	state, err := agents.PrepareStepTx(ctx, tx, l.Store, in.Authority, intent, 0, owners)
	if err != nil {
		return ReserveResult{}, err
	}
	if state != limits.DebitPrepared {
		return ReserveResult{}, limits.ErrDebitState
	}
	repeat, err := claimCallTx(ctx, tx, in, reservation)
	if err != nil {
		return ReserveResult{}, err
	}
	if repeat != nil {
		return ReserveResult{Repeat: repeat}, nil
	}
	if err = tx.Commit(ctx); err != nil {
		return ReserveResult{}, err
	}
	if err = l.Store.Admit(ctx, reservation.DispatchID, reservation.ClaimID, requests); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = l.Store.ReleaseUndispatched(releaseCtx, reservation.DispatchID, reservation.ClaimID)
		cancel()
		return ReserveResult{}, err
	}
	return ReserveResult{Reservation: reservation}, nil
}

func reservationPrice(reservation *Reservation) *string {
	if reservation.UnitPrice == nil {
		return nil
	}
	price := *reservation.UnitPrice
	return &price
}

func reservationCurrency(reservation *Reservation) *string {
	if reservation.Currency == "" {
		return nil
	}
	currency := reservation.Currency
	return &currency
}

func (l *DurableLedger) recordRefusedUnpriced(ctx context.Context, in ReserveInput, reservation *Reservation, intent limits.DebitIntent, boundaries access.ToolDebitBoundariesResult) error {
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = agents.StartRunTx(ctx, tx, l.Auth, in.Authority, reservation.RunID, "tools", in.ToolsetID, in.ToolsetRevisionID); err != nil {
		return err
	}
	if err = agents.EnsureBoundaryAccountsTx(ctx, tx, in.Authority, boundaries); err != nil {
		return err
	}
	state, err := agents.PrepareStepTx(ctx, tx, l.Store, in.Authority, intent, 0, boundaries.Owners())
	if err != nil {
		return err
	}
	if state != limits.DebitPrepared {
		return limits.ErrDebitState
	}
	now := time.Now().UTC()
	if _, err = tx.Exec(ctx, `INSERT INTO olp.mcp_tool_calls(id,dispatch_id,api_key_id,project_id,toolset_id,toolset_revision_id,server_id,server_revision_id,tool_alias,input_digest,call_digest,result_digest,status,is_error,started_at,completed_at,unit_price,pricing_state,currency) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULL,'refused_unpriced',false,$12,$12,NULL,'unpriced',$13)`,
		reservation.AttemptID, reservation.DispatchID, reservation.APIKeyID, reservation.ProjectID,
		reservation.ToolsetID, reservation.ToolsetRevisionID, in.ServerID, in.ServerRevisionID,
		reservation.ToolAlias, reservation.InputDigest, reservation.CallDigest, now,
		reservationCurrency(reservation)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return l.Store.ReleaseUndispatched(releaseCtx, reservation.DispatchID, reservation.ClaimID)
}

func claimCallTx(ctx context.Context, tx pgx.Tx, in ReserveInput, reservation *Reservation) (*RetainedCall, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO olp.mcp_call_identities(api_key_id,project_id,call_digest,dispatch_id,attempt_id,toolset_id,toolset_revision_id,tool_alias,input_digest,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp()+make_interval(secs=>$10)) ON CONFLICT(api_key_id,call_digest) DO NOTHING`,
		reservation.APIKeyID, reservation.ProjectID, in.CallDigest, reservation.DispatchID, reservation.AttemptID,
		in.ToolsetID, in.ToolsetRevisionID, in.ToolAlias, in.InputDigest, callIdentityBinding.Seconds())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var existing struct {
		dispatchID string
		attemptID  string
		projectID  string
		revisionID string
		alias      string
		input      string
		expired    bool
		state      limits.DebitState
		status     Status
	}
	err = tx.QueryRow(ctx, `SELECT i.dispatch_id::text,i.attempt_id::text,i.project_id::text,i.toolset_revision_id::text,i.tool_alias,i.input_digest,i.expires_at<=clock_timestamp(),d.state,COALESCE(c.status,'')
FROM olp.mcp_call_identities i JOIN olp.execution_debits d ON d.id=i.dispatch_id LEFT JOIN olp.mcp_tool_calls c ON c.dispatch_id=i.dispatch_id
WHERE i.api_key_id=$1 AND i.call_digest=$2 FOR UPDATE OF i`, reservation.APIKeyID, in.CallDigest).
		Scan(&existing.dispatchID, &existing.attemptID, &existing.projectID, &existing.revisionID, &existing.alias, &existing.input, &existing.expired, &existing.state, &existing.status)
	if err != nil {
		return nil, err
	}
	if existing.expired || existing.state == limits.DebitReleased && existing.status == "" {
		_, err = tx.Exec(ctx, `UPDATE olp.mcp_call_identities SET project_id=$3,dispatch_id=$4,attempt_id=$5,toolset_id=$6,toolset_revision_id=$7,tool_alias=$8,input_digest=$9,created_at=clock_timestamp(),expires_at=clock_timestamp()+make_interval(secs=>$10) WHERE api_key_id=$1 AND call_digest=$2`,
			reservation.APIKeyID, in.CallDigest, reservation.ProjectID, reservation.DispatchID, reservation.AttemptID,
			in.ToolsetID, in.ToolsetRevisionID, in.ToolAlias, in.InputDigest, callIdentityBinding.Seconds())
		return nil, err
	}
	status := existing.status
	switch status {
	case StatusSucceeded, StatusUpstreamError, StatusOutcomeUnknown, StatusSettled, StatusNotAccepted, StatusGuardrailBlocked:
	case StatusDispatched:
		status = StatusInProgress
	default:
		if existing.state == limits.DebitPrepared || existing.state == limits.DebitReady || existing.state == limits.DebitDispatched {
			status = StatusInProgress
		} else {
			status = StatusOutcomeUnknown
		}
	}
	return &RetainedCall{
		AttemptID:  existing.attemptID,
		DispatchID: existing.dispatchID,
		Status:     status,
		Conflict:   existing.projectID != reservation.ProjectID || existing.revisionID != in.ToolsetRevisionID || existing.alias != in.ToolAlias || existing.input != in.InputDigest,
	}, nil
}

func (l *DurableLedger) MarkDispatched(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error {
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = limits.MarkDispatchedTx(ctx, tx, reservation.DispatchID, reservation.ClaimID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.mcp_tool_calls(id,dispatch_id,api_key_id,project_id,toolset_id,toolset_revision_id,server_id,server_revision_id,tool_alias,input_digest,call_digest,result_digest,status,is_error,started_at,completed_at,unit_price,pricing_state,currency) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULL,'dispatched',false,$12,NULL,$13::numeric,$14,$15) ON CONFLICT(dispatch_id) DO NOTHING`,
		reservation.AttemptID, reservation.DispatchID, reservation.APIKeyID, reservation.ProjectID,
		reservation.ToolsetID, reservation.ToolsetRevisionID, evidence.ServerID, evidence.ServerRevisionID,
		reservation.ToolAlias, reservation.InputDigest, reservation.CallDigest, evidence.StartedAt,
		reservationPrice(reservation), pricingStateFor(reservation.UnitPrice), reservationCurrency(reservation)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l *DurableLedger) RecoverOrphanedDispatches(ctx context.Context, staleBefore time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 256
	}
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT d.id::text FROM olp.execution_debits d JOIN olp.mcp_tool_calls c ON c.dispatch_id=d.id WHERE d.kind='tool' AND d.state='dispatched' AND c.status='dispatched' AND d.dispatched_at<=$1 ORDER BY d.dispatched_at LIMIT $2 FOR UPDATE OF d,c SKIP LOCKED`, staleBefore, limit)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.Exec(ctx, `UPDATE olp.mcp_tool_calls SET status='outcome_unknown',completed_at=clock_timestamp() WHERE dispatch_id=$1 AND status='dispatched'`, id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='outcome_unknown' WHERE id=$1 AND state='dispatched'`, id); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (l *DurableLedger) ExpireCallIdentities(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 256
	}
	tag, err := l.Pool.Exec(ctx, `DELETE FROM olp.mcp_call_identities WHERE ctid IN (SELECT ctid FROM olp.mcp_call_identities WHERE expires_at<=clock_timestamp() ORDER BY expires_at LIMIT $1)`, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (l *DurableLedger) ReleaseUndispatched(ctx context.Context, reservation *Reservation) error {
	return l.Store.ReleaseUndispatched(ctx, reservation.DispatchID, reservation.ClaimID)
}

func evidenceDecisions(evidence AttemptEvidence) []byte {
	if len(evidence.GuardrailDecisions) == 0 {
		return []byte("[]")
	}
	raw, err := json.Marshal(evidence.GuardrailDecisions)
	if err != nil {
		return []byte("[]")
	}
	return raw
}

func (l *DurableLedger) RefuseUndispatched(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error {
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO olp.mcp_tool_calls(id,dispatch_id,api_key_id,project_id,toolset_id,toolset_revision_id,server_id,server_revision_id,tool_alias,input_digest,call_digest,result_digest,status,is_error,started_at,completed_at,unit_price,pricing_state,currency,guardrail_decisions,identity_decisions) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULL,'guardrail_blocked',false,$12,$13,$14::numeric,$15,$16,$17,$18)`,
		reservation.AttemptID, reservation.DispatchID, reservation.APIKeyID, reservation.ProjectID,
		reservation.ToolsetID, reservation.ToolsetRevisionID, evidence.ServerID, evidence.ServerRevisionID,
		reservation.ToolAlias, reservation.InputDigest, reservation.CallDigest, evidence.StartedAt, evidence.CompletedAt,
		reservationPrice(reservation), pricingStateFor(reservation.UnitPrice), reservationCurrency(reservation), evidenceDecisions(evidence), identityEvidence(evidence.IdentityDecisions)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return l.Store.ReleaseUndispatched(releaseCtx, reservation.DispatchID, reservation.ClaimID)
}

func insertAttemptTx(ctx context.Context, tx pgx.Tx, reservation *Reservation, evidence AttemptEvidence) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.mcp_tool_calls(id,dispatch_id,api_key_id,project_id,toolset_id,toolset_revision_id,server_id,server_revision_id,tool_alias,input_digest,call_digest,result_digest,status,is_error,started_at,completed_at,unit_price,pricing_state,currency,guardrail_decisions,identity_decisions) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::numeric,$18,$19,$20,$21) ON CONFLICT(dispatch_id) DO UPDATE SET result_digest=EXCLUDED.result_digest,status=EXCLUDED.status,is_error=EXCLUDED.is_error,completed_at=EXCLUDED.completed_at,guardrail_decisions=EXCLUDED.guardrail_decisions,identity_decisions=CASE WHEN jsonb_array_length(EXCLUDED.identity_decisions)=0 THEN mcp_tool_calls.identity_decisions ELSE EXCLUDED.identity_decisions END WHERE mcp_tool_calls.status='dispatched'`,
		reservation.AttemptID, reservation.DispatchID, reservation.APIKeyID, reservation.ProjectID,
		reservation.ToolsetID, reservation.ToolsetRevisionID, evidence.ServerID, evidence.ServerRevisionID,
		reservation.ToolAlias, reservation.InputDigest, reservation.CallDigest, evidence.ResultDigest, string(evidence.Status), evidence.IsError,
		evidence.StartedAt, evidence.CompletedAt,
		reservationPrice(reservation), pricingStateFor(reservation.UnitPrice), reservationCurrency(reservation), evidenceDecisions(evidence), identityEvidence(evidence.IdentityDecisions))
	return err
}

func (l *DurableLedger) MarkUnknown(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error {
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = insertAttemptTx(ctx, tx, reservation, evidence); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.execution_debits SET state='outcome_unknown' WHERE id=$1 AND claim_id=$2 AND state IN ('dispatched','outcome_unknown')`, reservation.DispatchID, reservation.ClaimID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		var state limits.DebitState
		if scanErr := tx.QueryRow(ctx, `SELECT state FROM olp.execution_debits WHERE id=$1`, reservation.DispatchID).Scan(&state); scanErr == nil && state == limits.DebitSettled {
			return tx.Commit(ctx)
		}
		return limits.ErrDebitState
	}
	return tx.Commit(ctx)
}

func (l *DurableLedger) Settle(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error {
	resultDigest := sha256.Sum256([]byte("olp:mcp-tool-result:" + reservation.DispatchID + ":" + reservation.AttemptID))
	evidenceDigest := sha256.Sum256([]byte(reservation.AttemptID + ":" + string(evidence.Status) + ":" + evidence.ResultDigest))
	result := limits.DebitResult{
		DispatchID: reservation.DispatchID, ResultDigest: resultDigest[:], EvidenceDigest: evidenceDigest[:],
		EventID: reservation.AttemptID, Cost: reservation.amount(), Charge: reservation.amount(), Final: true, Complete: true,
		Unpriced: reservation.UnitPrice == nil, ObservedAt: evidence.CompletedAt,
	}
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = insertAttemptTx(ctx, tx, reservation, evidence); err != nil {
		return err
	}
	if err = limits.SettleToolDebitTx(ctx, tx, result); err != nil {
		if errors.Is(err, limits.ErrDebitState) {
			var settled bool
			if scanErr := tx.QueryRow(ctx, `SELECT state='settled' FROM olp.execution_debits WHERE id=$1`, reservation.DispatchID).Scan(&settled); scanErr == nil && settled {
				return tx.Commit(ctx)
			}
		}
		return err
	}
	return tx.Commit(ctx)
}

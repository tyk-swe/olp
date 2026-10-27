package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
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

func (l *DurableLedger) Reserve(ctx context.Context, in ReserveInput) (*Reservation, error) {
	if l.Pool == nil || l.Store == nil || l.Auth == nil || in.Authority.ProjectID == nil {
		return nil, limits.ErrDebitAuthority
	}
	ids := make([]string, 5)
	for i := range ids {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	reservation := &Reservation{
		DispatchID: ids[0], ClaimID: ids[1], RunID: ids[2], StepID: ids[3], AttemptID: ids[4],
		APIKeyID: in.Authority.ID, ProjectID: *in.Authority.ProjectID,
		ToolsetID: in.ToolsetID, ToolsetRevisionID: in.ToolsetRevisionID,
		ToolAlias: in.ToolAlias, InputDigest: in.InputDigest,
	}
	currency, err := l.currency(ctx)
	if err != nil || currency == "" {
		return nil, limits.ErrDebitAuthority
	}
	intent := limits.DebitIntent{
		ID: reservation.DispatchID, APIKeyID: in.Authority.ID, ProjectID: reservation.ProjectID,
		RunID: reservation.RunID, StepID: reservation.StepID, Kind: "tool", ClaimID: reservation.ClaimID,
		Currency: currency, CostBound: "0", ChargeBound: "0", PreparedUntil: time.Now().Add(90 * time.Second),
		Affinity: limits.ExecutionAffinity{ReleaseID: in.ReleaseID, RevisionID: in.ToolsetRevisionID, ToolAlias: in.ToolAlias, InputDigest: in.InputDigest},
	}
	owners := []limits.DebitOwner{in.Authority.KeyDebitOwner()}
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = agents.StartRunTx(ctx, tx, l.Auth, in.Authority, reservation.RunID, "tools", in.ToolsetID, in.ToolsetRevisionID); err != nil {
		return nil, err
	}
	state, err := agents.PrepareStepTx(ctx, tx, l.Store, in.Authority, intent, 0, owners)
	if err != nil {
		return nil, err
	}
	if state != limits.DebitPrepared {
		return nil, limits.ErrDebitState
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	if err = l.Store.Admit(ctx, reservation.DispatchID, reservation.ClaimID, []limits.Request{in.Authority.KeyDebitRequest(time.Hour)}); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = l.Store.ReleaseUndispatched(releaseCtx, reservation.DispatchID, reservation.ClaimID)
		cancel()
		return nil, err
	}
	return reservation, nil
}

func (l *DurableLedger) MarkDispatched(ctx context.Context, reservation *Reservation) error {
	return l.Store.MarkDispatched(ctx, reservation.DispatchID, reservation.ClaimID)
}

func (l *DurableLedger) ReleaseUndispatched(ctx context.Context, reservation *Reservation) error {
	return l.Store.ReleaseUndispatched(ctx, reservation.DispatchID, reservation.ClaimID)
}

func insertAttemptTx(ctx context.Context, tx pgx.Tx, reservation *Reservation, evidence AttemptEvidence) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.mcp_tool_calls(id,dispatch_id,api_key_id,project_id,toolset_id,toolset_revision_id,server_id,server_revision_id,tool_alias,input_digest,result_digest,status,is_error,started_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(dispatch_id) DO NOTHING`,
		reservation.AttemptID, reservation.DispatchID, reservation.APIKeyID, reservation.ProjectID,
		reservation.ToolsetID, reservation.ToolsetRevisionID, evidence.ServerID, evidence.ServerRevisionID,
		reservation.ToolAlias, reservation.InputDigest, evidence.ResultDigest, string(evidence.Status), evidence.IsError,
		evidence.StartedAt, evidence.CompletedAt)
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
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return l.Store.MarkUnknown(ctx, reservation.DispatchID, reservation.ClaimID)
}

func (l *DurableLedger) Settle(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error {
	resultDigest := sha256.Sum256([]byte("olp:mcp-tool-result:" + reservation.DispatchID + ":" + reservation.AttemptID))
	evidenceDigest := sha256.Sum256([]byte(reservation.AttemptID + ":" + string(evidence.Status) + ":" + evidence.ResultDigest))
	result := limits.DebitResult{
		DispatchID: reservation.DispatchID, ResultDigest: resultDigest[:], EvidenceDigest: evidenceDigest[:],
		EventID: reservation.AttemptID, Cost: "0", Charge: "0", Final: true, Complete: true,
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

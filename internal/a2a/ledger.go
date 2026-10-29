package a2a

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Ledger struct {
	Pool  *pgxpool.Pool
	Store *limits.DebitStore
	Auth  *secrets.AuthKey
}

type reservation struct {
	DispatchID string
	ClaimID    string
	RunID      string
	StepID     string
	EventID    string
	Price      string
}

type task struct {
	ID         string
	UpstreamID string
	ContextID  string
	RevisionID string
}

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func newReservation(price string) (reservation, error) {
	dispatch, dispatchErr := newID()
	claim, claimErr := newID()
	run, runErr := newID()
	step, stepErr := newID()
	event, eventErr := newID()
	if err := errors.Join(dispatchErr, claimErr, runErr, stepErr, eventErr); err != nil {
		return reservation{}, err
	}
	return reservation{DispatchID: dispatch, ClaimID: claim, RunID: run, StepID: step, EventID: event, Price: price}, nil
}

func releaseAffinity(release *runtime.Release) string {
	if id, err := uuid.Parse(release.ID); err == nil && id.String() == release.ID && id != uuid.Nil {
		return release.ID
	}
	return release.Snapshot.Generation.ID
}

func (l *Ledger) reserve(ctx context.Context, authority access.Authority, revision agentcontract.Revision, release *runtime.Release) (reservation, error) {
	if l == nil || l.Pool == nil || l.Store == nil || l.Auth == nil || authority.ProjectID == nil || revision.Definition.Agent == nil || revision.Definition.Agent.MessagePrice == "" || release == nil || release.Snapshot == nil {
		return reservation{}, limits.ErrDebitAuthority
	}
	boundaries := authority.ToolDebitBoundaries(time.Hour)
	if !boundaries.Resolved {
		return reservation{}, limits.ErrDebitAuthority
	}
	res, err := newReservation(revision.Definition.Agent.MessagePrice)
	if err != nil {
		return reservation{}, err
	}
	var currency string
	if err := l.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')`).Scan(&currency); err != nil || currency == "" {
		return reservation{}, limits.ErrDebitAuthority
	}
	releaseID := releaseAffinity(release)
	price := revision.Definition.Agent.MessagePrice
	intent := limits.DebitIntent{ID: res.DispatchID, APIKeyID: authority.ID, ProjectID: *authority.ProjectID, RunID: res.RunID, StepID: res.StepID, Kind: "agent", ClaimID: res.ClaimID, Currency: currency, CostBound: price, ChargeBound: price, PreparedUntil: time.Now().Add(90 * time.Second), Affinity: limits.ExecutionAffinity{ReleaseID: releaseID, RevisionID: revision.ID, PricingRevisionID: revision.Definition.Agent.PricingRevisionID}}
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return reservation{}, err
	}
	defer tx.Rollback(ctx)
	if err := agents.StartRunTx(ctx, tx, l.Auth, authority, res.RunID, "agents", revision.PublicationID, revision.ID); err != nil {
		return reservation{}, err
	}
	if err := agents.EnsureBoundaryAccountsTx(ctx, tx, authority, boundaries); err != nil {
		return reservation{}, err
	}
	state, err := agents.PrepareStepTx(ctx, tx, l.Store, authority, intent, 0, boundaries.Owners())
	if err != nil {
		return reservation{}, err
	}
	if state != limits.DebitPrepared {
		return reservation{}, limits.ErrDebitState
	}
	if err := tx.Commit(ctx); err != nil {
		return reservation{}, err
	}
	if err := l.Store.Admit(ctx, res.DispatchID, res.ClaimID, boundaries.Requests()); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = l.Store.ReleaseUndispatched(releaseCtx, res.DispatchID, res.ClaimID)
		cancel()
		return reservation{}, err
	}
	return res, nil
}

func (l *Ledger) findTask(ctx context.Context, authority access.Authority, publicationID, rawID string) (task, error) {
	if l == nil || l.Pool == nil || authority.ProjectID == nil {
		return task{}, pgx.ErrNoRows
	}
	id, err := uuid.Parse(rawID)
	if err != nil || id == uuid.Nil || id.String() != rawID {
		return task{}, pgx.ErrNoRows
	}
	var found task
	if err := l.Pool.QueryRow(ctx, `SELECT id::text,upstream_id,upstream_context_id,revision_id::text FROM olp.a2a_tasks WHERE id=$1 AND api_key_id=$2 AND project_id=$3 AND publication_id=$4`, rawID, authority.ID, *authority.ProjectID, publicationID).Scan(&found.ID, &found.UpstreamID, &found.ContextID, &found.RevisionID); err != nil {
		return task{}, err
	}
	return found, nil
}

func (l *Ledger) settle(ctx context.Context, res reservation, authority access.Authority, revision agentcontract.Revision, upstreamID, contextID string) (string, error) {
	if authority.ProjectID == nil || len(upstreamID) > 1024 || len(contextID) > 1024 || strings.ContainsRune(upstreamID, 0) || strings.ContainsRune(contextID, 0) {
		return "", limits.ErrDebitConflict
	}
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	taskID, err := settleTask(ctx, tx, res, authority, revision, upstreamID, contextID)
	if err != nil {
		return "", err
	}
	resultDigest := sha256.Sum256([]byte("olp:a2a-result:" + res.DispatchID))
	evidenceDigest := sha256.Sum256([]byte("olp:a2a-settled:" + res.DispatchID))
	result := limits.DebitResult{DispatchID: res.DispatchID, ResultDigest: resultDigest[:], EvidenceDigest: evidenceDigest[:], EventID: res.EventID, Cost: res.Price, Charge: res.Price, Final: true, Complete: true, ObservedAt: time.Now().UTC()}
	if err := limits.SettleAgentDebitTx(ctx, tx, result); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return taskID, nil
}

func settleTask(ctx context.Context, tx pgx.Tx, res reservation, authority access.Authority, revision agentcontract.Revision, upstreamID, contextID string) (string, error) {
	if upstreamID == "" {
		return "", nil
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.a2a_tasks(id,api_key_id,project_id,publication_id,revision_id,dispatch_id,upstream_id,upstream_context_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(api_key_id,project_id,publication_id,upstream_id) DO NOTHING`, id, authority.ID, *authority.ProjectID, revision.PublicationID, revision.ID, res.DispatchID, upstreamID, contextID); err != nil {
		return "", err
	}
	var publicID, pinned string
	if err := tx.QueryRow(ctx, `SELECT id::text,revision_id::text FROM olp.a2a_tasks WHERE api_key_id=$1 AND project_id=$2 AND publication_id=$3 AND upstream_id=$4`, authority.ID, *authority.ProjectID, revision.PublicationID, upstreamID).Scan(&publicID, &pinned); err != nil {
		return "", err
	}
	if pinned != revision.ID {
		return "", limits.ErrDebitConflict
	}
	return publicID, nil
}

func (l *Ledger) release(ctx context.Context, res reservation) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = l.Store.ReleaseUndispatched(releaseCtx, res.DispatchID, res.ClaimID)
}

func (l *Ledger) unknown(ctx context.Context, res reservation) {
	unknownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.Store.MarkUnknown(unknownCtx, res.DispatchID, res.ClaimID); err != nil && !errors.Is(err, limits.ErrDebitState) {
		return
	}
}

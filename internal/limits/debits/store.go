package debits

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func within[T any](ctx context.Context, pool *pgxpool.Pool, run func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	value, err := run(tx)
	if err != nil {
		return zero, err
	}
	return value, tx.Commit(ctx)
}

func (s *Store) inTx(ctx context.Context, run func(pgx.Tx) error) error {
	_, err := within(ctx, s.pool, func(tx pgx.Tx) (struct{}, error) { return struct{}{}, run(tx) })
	return err
}

func ownerIDs(owners []limits.Owner) []string {
	ids := make([]string, 0, len(owners))
	for _, o := range owners {
		ids = append(ids, o.ID)
	}
	return slices.Compact(ids)
}

func lockOwners(ctx context.Context, tx pgx.Tx, owners []limits.Owner) error {
	for _, id := range ownerIDs(owners) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('olp:execution-debit:'||$1,0))`, id); err != nil {
			return err
		}
	}
	return nil
}

func bumpOwners(ctx context.Context, tx pgx.Tx, owners []limits.Owner) error {
	for _, id := range ownerIDs(owners) {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_versions(owner_id,version) VALUES($1,1) ON CONFLICT(owner_id) DO UPDATE SET version=olp.execution_debit_versions.version+1`, id); err != nil {
			return err
		}
	}
	return nil
}

func ownersTx(ctx context.Context, tx pgx.Tx, id string) ([]limits.Owner, error) {
	rows, err := tx.Query(ctx, `SELECT owner_id::text,owner_kind,basis FROM olp.execution_debit_owners WHERE dispatch_id=$1 ORDER BY owner_id,basis`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (limits.Owner, error) {
		var o limits.Owner
		err := row.Scan(&o.ID, &o.Kind, &o.Basis)
		return o, err
	})
}

func (s *Store) Prepare(ctx context.Context, i Intent, owners []limits.Owner) (State, error) {
	return within(ctx, s.pool, func(tx pgx.Tx) (State, error) { return PrepareTx(ctx, tx, i, owners) })
}

func PrepareTx(ctx context.Context, tx pgx.Tx, i Intent, owners []limits.Owner) (State, error) {
	if err := i.validate(); err != nil {
		return "", err
	}
	normalized, err := normalizedOwners(owners)
	if err != nil {
		return "", err
	}
	now := time.Now()
	ownsKey := slices.ContainsFunc(normalized, func(o limits.Owner) bool { return o.Kind == limits.OwnerKey && o.ID == i.APIKeyID })
	if !ownsKey || !i.PreparedUntil.After(now) || i.PreparedUntil.After(now.Add(2*time.Minute)) {
		return "", ErrConflict
	}
	var authorized bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.api_keys WHERE id=$1 AND project_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) AND COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')=$3)`, i.APIKeyID, i.ProjectID, i.Currency).Scan(&authorized); err != nil {
		return "", err
	}
	if !authorized {
		return "", ErrAuthority
	}
	digest, err := intentDigest(i, normalized)
	if err != nil {
		return "", err
	}
	affinity, err := json.Marshal(i.Affinity)
	if err != nil {
		return "", errors.Join(ErrConflict, err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO olp.execution_debits(id,api_key_id,project_id,run_id,step_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,prepared_until)
 VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11::numeric,$12::numeric,$13) ON CONFLICT(id) DO NOTHING`, i.ID, i.APIKeyID, i.ProjectID, i.RunID, i.StepID, i.Kind, affinity, digest[:], i.ClaimID, i.Currency, i.CostBound, i.ChargeBound, i.PreparedUntil)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return replayedState(ctx, tx, i, digest[:])
	}
	if err := lockOwners(ctx, tx, normalized); err != nil {
		return "", err
	}
	w, err := ledger.CurrentBudgetWindows(ctx, tx, now)
	if err != nil {
		return "", err
	}
	for _, o := range normalized {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_owners(dispatch_id,owner_id,owner_kind,basis,bound,daily_window_id,weekly_window_id,monthly_window_id) VALUES($1,$2,$3,$4,$5::numeric,$6,$7,$8)`, i.ID, o.ID, o.Kind, o.Basis, o.Basis.Pick(i.CostBound, i.ChargeBound), w.DailyID, w.WeeklyID, w.MonthlyID); err != nil {
			return "", err
		}
	}
	return Prepared, bumpOwners(ctx, tx, normalized)
}

func replayedState(ctx context.Context, tx pgx.Tx, i Intent, digest []byte) (State, error) {
	var existing []byte
	var state State
	if err := tx.QueryRow(ctx, `SELECT intent_digest,state FROM olp.execution_debits WHERE id=$1 AND api_key_id=$2`, i.ID, i.APIKeyID).Scan(&existing, &state); err != nil || !bytes.Equal(existing, digest) {
		return "", ErrConflict
	}
	return state, nil
}

func (s *Store) MarkDispatched(ctx context.Context, id, claim string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return MarkDispatchedTx(ctx, tx, id, claim) })
}

func MarkDispatchedTx(ctx context.Context, tx pgx.Tx, id, claim string) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.execution_debits d SET state='dispatched',dispatched_at=clock_timestamp()
 WHERE d.id=$1 AND d.claim_id=$2 AND d.state='ready' AND d.prepared_until>clock_timestamp()
 AND EXISTS(SELECT 1 FROM olp.api_keys k WHERE k.id=d.api_key_id AND k.project_id IS NOT DISTINCT FROM d.project_id AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()))`, id, claim)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrState
	}
	return nil
}

func (s *Store) HasUnknownDebit(ctx context.Context, apiKeyID string, maxDispatch time.Duration) (bool, error) {
	if !canonicalID(apiKeyID) {
		return false, ErrConflict
	}
	window := maxDispatch
	if window <= 0 {
		window = time.Hour
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.execution_debits d WHERE d.api_key_id=$1 AND (d.state='outcome_unknown' OR (d.state='dispatched' AND d.dispatched_at<clock_timestamp()-make_interval(secs=>$2))) AND d.closed_at IS NULL AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_results r WHERE r.dispatch_id=d.id AND r.final AND (d.kind<>'model' OR r.complete) AND r.usage_complete))`, apiKeyID, window.Seconds()).Scan(&held)
	return held, err
}

func (s *Store) MarkUnknown(ctx context.Context, id, claim string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.execution_debits SET state='outcome_unknown' WHERE id=$1 AND claim_id=$2 AND state IN ('dispatched','outcome_unknown')`, id, claim)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrState
	}
	return err
}

func claimedState(ctx context.Context, tx pgx.Tx, id, claim string) (State, error) {
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM olp.execution_debits WHERE id=$1 AND claim_id=$2 FOR UPDATE`, id, claim).Scan(&state); err != nil {
		return "", ErrState
	}
	return state, nil
}

func closeTx(ctx context.Context, tx pgx.Tx, ids []string, closing string) error {
	owners := []limits.Owner{}
	for _, id := range ids {
		owned, err := ownersTx(ctx, tx, id)
		if err != nil {
			return err
		}
		owners = append(owners, owned...)
	}
	slices.SortStableFunc(owners, func(a, b limits.Owner) int { return cmp.Compare(a.ID, b.ID) })
	if err := lockOwners(ctx, tx, owners); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, closing, ids); err != nil {
		return err
	}
	return bumpOwners(ctx, tx, owners)
}

const (
	releaseSQL     = `UPDATE olp.execution_debits SET state='released',closed_at=clock_timestamp() WHERE id=ANY($1::uuid[])`
	notAcceptedSQL = `UPDATE olp.execution_debits SET state='not_accepted',closed_at=clock_timestamp() WHERE id=ANY($1::uuid[])`
)

func (s *Store) ReleaseUndispatched(ctx context.Context, id, claim string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		state, err := claimedState(ctx, tx, id, claim)
		switch {
		case err != nil:
			return err
		case state == Released:
			return nil
		case state != Prepared && state != Ready:
			return ErrState
		}
		return closeTx(ctx, tx, []string{id}, releaseSQL)
	})
}

func (s *Store) MarkNotSent(ctx context.Context, id, claim string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		state, err := claimedState(ctx, tx, id, claim)
		switch {
		case err != nil:
			return err
		case state == NotAccepted:
			return nil
		case state == Unknown:
			if err := proveNotSentTx(ctx, tx, id, claim); err != nil {
				return err
			}
		case state != Dispatched:
			return ErrState
		}
		return closeTx(ctx, tx, []string{id}, notAcceptedSQL)
	})
}

func proveNotSentTx(ctx context.Context, tx pgx.Tx, id, claim string) error {
	proof, err := tx.Exec(ctx, `INSERT INTO olp.workflow_model_nonacceptance_proofs(dispatch_id,claim_id,source)
			SELECT d.id,d.claim_id,'transport_not_sent' FROM olp.execution_debits d
			JOIN olp.workflow_model_leases l ON l.dispatch_id=d.id AND l.claim_id=d.claim_id
			WHERE d.id=$1 AND d.claim_id=$2 AND d.kind='model' AND d.run_id IS NOT NULL
			  AND d.observed_cost=0 AND d.observed_charge=0
			  AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_results r
			    WHERE r.dispatch_id=d.id AND (r.cost<>0 OR r.charge<>0 OR r.complete))`, id, claim)
	if err != nil {
		return err
	}
	if proof.RowsAffected() != 1 {
		return ErrState
	}
	return nil
}

func (s *Store) RecoverPrepared(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 256 {
		return 0, errors.New("invalid debit recovery limit")
	}
	return within(ctx, s.pool, func(tx pgx.Tx) (int, error) {
		rows, err := tx.Query(ctx, `SELECT id::text FROM olp.execution_debits WHERE state IN ('prepared','ready') AND prepared_until<clock_timestamp() ORDER BY prepared_until,id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return 0, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			return 0, err
		}
		return len(ids), closeTx(ctx, tx, ids, releaseSQL)
	})
}

package debits

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

type Result struct {
	DispatchID                   string
	ResultDigest, EvidenceDigest []byte
	EventID                      string
	Cost, Charge                 string
	Final, Complete              bool
	Unpriced                     bool
	ObservedAt                   time.Time
	ResolvesResultDigests        [][]byte
}

func ResultReplayTx(ctx context.Context, tx pgx.Tx, r Result) (bool, error) {
	if !canonicalID(r.DispatchID) || !canonicalID(r.EventID) || len(r.ResultDigest) != 32 || len(r.EvidenceDigest) != 32 || !validAmount(r.Cost) || !validAmount(r.Charge) {
		return false, ErrConflict
	}
	resolution, err := resolutionDigest(r.ResolvesResultDigests)
	if err != nil || (!r.Complete && len(r.ResolvesResultDigests) > 0) {
		return false, ErrConflict
	}
	var evidence, resolutions []byte
	var event, cost, charge string
	var final, complete bool
	err = tx.QueryRow(ctx, `SELECT evidence_digest,resolution_digest,event_id::text,cost::text,charge::text,final,complete FROM olp.execution_debit_results WHERE dispatch_id=$1 AND result_digest=$2`, r.DispatchID, r.ResultDigest).Scan(&evidence, &resolutions, &event, &cost, &charge, &final, &complete)
	if err == nil {
		if !resultMatches(r, resolution, evidence, resolutions, event, cost, charge, final, complete) {
			return false, ErrConflict
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return false, nil
}

func resultMatches(r Result, resolution, evidence, resolutions []byte, event, cost, charge string, final, complete bool) bool {
	return bytes.Equal(evidence, r.EvidenceDigest) && bytes.Equal(resolutions, resolution) && event == r.EventID && limits.AmountEqual(cost, r.Cost) && limits.AmountEqual(charge, r.Charge) && final == r.Final && complete == r.Complete
}

type settleHead struct {
	state          State
	kind           Kind
	costBound      string
	chargeBound    string
	observedCost   string
	observedCharge string
}

func beginSettleTx(ctx context.Context, tx pgx.Tx, r Result) (settleHead, bool, error) {
	var head settleHead
	if err := tx.QueryRow(ctx, `SELECT state,kind,cost_bound::text,charge_bound::text,observed_cost::text,observed_charge::text FROM olp.execution_debits WHERE id=$1 FOR UPDATE`, r.DispatchID).Scan(&head.state, &head.kind, &head.costBound, &head.chargeBound, &head.observedCost, &head.observedCharge); err != nil {
		return head, false, err
	}
	duplicate, err := ResultReplayTx(ctx, tx, r)
	return head, duplicate, err
}

func appendResultTx(ctx context.Context, tx pgx.Tx, r Result, owners []limits.Owner, resolution []byte) error {
	if err := lockOwners(ctx, tx, owners); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_results(dispatch_id,result_digest,evidence_digest,event_id,cost,charge,final,complete,resolution_digest,usage_complete)
 SELECT d.id,$2,$3,$4,$5::numeric,$6::numeric,$7,$8,$9,COALESCE((SELECT bool_and(f.usage_complete) FROM olp.attempt_usage_facts f WHERE f.event_id=$4 AND (d.kind<>'model' OR f.attempt_id=d.id)),false)
 FROM olp.execution_debits d WHERE d.id=$1`, r.DispatchID, r.ResultDigest, r.EvidenceDigest, r.EventID, r.Cost, r.Charge, r.Final, r.Complete, resolution)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrState
	}
	return err
}

func SettleTx(ctx context.Context, tx pgx.Tx, r Result, accounting []limits.Snapshot) error {
	head, duplicate, err := beginSettleTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	resolution, err := resolutionDigest(r.ResolvesResultDigests)
	if err != nil {
		return err
	}
	if head.state != Dispatched && head.state != Unknown {
		return ErrState
	}
	var persisted bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT count(*)>0 AND sum(COALESCE(f.estimated_cost,0))=$3::numeric AND sum(COALESCE(f.charge,0))=$4::numeric AND bool_and(f.charge_status='not_billable' OR f.currency=d.currency OR (f.estimated_cost=0 AND f.charge=0) OR (NOT $5 AND f.currency IS NULL)) AND bool_and((d.affinity->>'pricing_revision_id' IS NULL OR f.pricing_revision_id::text=d.affinity->>'pricing_revision_id') AND (d.affinity->>'rate_card_revision_id' IS NULL OR f.rate_card_revision_id::text=d.affinity->>'rate_card_revision_id')) AND (NOT $5 OR bool_and(f.charge_status='not_billable' OR (f.usage_complete AND NOT f.unpriced AND f.estimated_cost IS NOT NULL AND f.charge IS NOT NULL))) FROM olp.attempt_usage_facts f JOIN olp.execution_debits d ON d.id=$2 WHERE f.event_id=$1 AND f.api_key_id=d.api_key_id AND (d.kind<>'model' OR f.attempt_id=d.id)),false)`, r.EventID, r.DispatchID, r.Cost, r.Charge, r.Complete).Scan(&persisted); err != nil {
		return err
	}
	if !persisted {
		return ErrState
	}
	owners, err := ownersTx(ctx, tx, r.DispatchID)
	if err != nil {
		return err
	}
	if !ownersAccounted(owners, accounting) {
		return ErrState
	}
	if err = appendResultTx(ctx, tx, r, owners, resolution); err != nil {
		return err
	}
	for _, resolved := range r.ResolvesResultDigests {
		tag, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_result_resolutions(dispatch_id,resolved_digest,evidence_result_digest) SELECT $1,$2,$3 WHERE $2::bytea<>$3::bytea AND EXISTS(SELECT 1 FROM olp.execution_debit_results WHERE dispatch_id=$1 AND result_digest=$2 AND NOT complete) AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_result_resolutions WHERE dispatch_id=$1 AND resolved_digest=$2)`, r.DispatchID, resolved, r.ResultDigest)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	var unresolved bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.execution_debit_results r WHERE r.dispatch_id=$1 AND NOT r.complete AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_result_resolutions x WHERE x.dispatch_id=r.dispatch_id AND x.resolved_digest=r.result_digest))`, r.DispatchID).Scan(&unresolved); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET
 observed_cost=observed_cost+$2::numeric,observed_charge=observed_charge+$3::numeric,
 bound_violation=bound_violation OR observed_cost+$2::numeric>cost_bound OR observed_charge+$3::numeric>charge_bound,
 state=CASE WHEN $4 AND $5 AND NOT $6 THEN 'settled' WHEN NOT $5 OR $6 THEN 'outcome_unknown' ELSE state END,
 closed_at=CASE WHEN $4 AND $5 AND NOT $6 THEN clock_timestamp() ELSE NULL END WHERE id=$1`, r.DispatchID, r.Cost, r.Charge, r.Final, r.Complete, unresolved)
	if err != nil {
		return err
	}
	return bumpOwners(ctx, tx, owners)
}

func ownersAccounted(owners []limits.Owner, accounting []limits.Snapshot) bool {
	for _, owner := range owners {
		basis := owner.Basis
		if owner.Kind == limits.OwnerSupply {
			basis = limits.BasisCost
		}
		if !slices.ContainsFunc(accounting, func(s limits.Snapshot) bool {
			return s.OwnerID == owner.ID && s.Basis.Normalized() == basis
		}) {
			return false
		}
	}
	return true
}

func resolutionDigest(values [][]byte) ([]byte, error) {
	if len(values) > 128 {
		return nil, ErrConflict
	}
	normalized := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) != 32 {
			return nil, ErrConflict
		}
		normalized = append(normalized, hex.EncodeToString(v))
	}
	slices.Sort(normalized)
	for i := 1; i < len(normalized); i++ {
		if normalized[i] == normalized[i-1] {
			return nil, ErrConflict
		}
	}
	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

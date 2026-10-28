package limits

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DebitStore owns monetary exposure, not provider lifecycles. Execution owners
// supply freshly authorized boundaries; workers never obtain dispatch claims.
type DebitStore struct {
	Pool    *pgxpool.Pool
	Limiter *Limiter

	reconcileMu    sync.Mutex
	reconcileAfter string
}

func lockDebitOwners(ctx context.Context, tx pgx.Tx, owners []DebitOwner) error {
	last := ""
	for _, o := range owners {
		if o.ID == last {
			continue
		}
		last = o.ID
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('olp:execution-debit:'||$1,0))`, o.ID); err != nil {
			return err
		}
	}
	return nil
}

func bumpDebitOwners(ctx context.Context, tx pgx.Tx, owners []DebitOwner) error {
	last := ""
	for _, o := range owners {
		if last == o.ID {
			continue
		}
		last = o.ID
		if _, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_versions(owner_id,version) VALUES($1,1) ON CONFLICT(owner_id) DO UPDATE SET version=olp.execution_debit_versions.version+1`, o.ID); err != nil {
			return err
		}
	}
	return nil
}

// Prepare commits the full finite intent before any admission or outbound IO.
// A replay finds the existing intent but cannot claim another process's fence.
func (s *DebitStore) Prepare(ctx context.Context, i DebitIntent, owners []DebitOwner) (DebitState, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	state, err := s.PrepareTx(ctx, tx, i, owners)
	if err != nil {
		return "", err
	}
	return state, tx.Commit(ctx)
}

// PrepareTx permits run/step references and the intent to commit together.
func (s *DebitStore) PrepareTx(ctx context.Context, tx pgx.Tx, i DebitIntent, owners []DebitOwner) (DebitState, error) {
	if err := i.validate(); err != nil {
		return "", err
	}
	owners, err := normalizedDebitOwners(owners)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(owners, func(o DebitOwner) bool { return o.Kind == "key" && o.ID == i.APIKeyID }) {
		return "", ErrDebitConflict
	}
	if !i.PreparedUntil.After(time.Now()) || i.PreparedUntil.After(time.Now().Add(2*time.Minute)) {
		return "", ErrDebitConflict
	}
	var authorized bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.api_keys WHERE id=$1 AND project_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) AND COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')=$3)`, i.APIKeyID, i.ProjectID, i.Currency).Scan(&authorized); err != nil {
		return "", err
	}
	if !authorized {
		return "", ErrDebitAuthority
	}
	digest := debitDigest(i, owners)
	affinity, _ := json.Marshal(i.Affinity)
	tag, err := tx.Exec(ctx, `INSERT INTO olp.execution_debits(id,api_key_id,project_id,run_id,step_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,prepared_until)
 VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11::numeric,$12::numeric,$13) ON CONFLICT(id) DO NOTHING`, i.ID, i.APIKeyID, i.ProjectID, i.RunID, i.StepID, i.Kind, affinity, digest[:], i.ClaimID, i.Currency, i.CostBound, i.ChargeBound, i.PreparedUntil)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		var existing []byte
		var state DebitState
		if err = tx.QueryRow(ctx, `SELECT intent_digest,state FROM olp.execution_debits WHERE id=$1 AND api_key_id=$2`, i.ID, i.APIKeyID).Scan(&existing, &state); err != nil {
			return "", ErrDebitConflict
		}
		if !bytes.Equal(existing, digest[:]) {
			return "", ErrDebitConflict
		}
		return state, nil
	}
	if err = lockDebitOwners(ctx, tx, owners); err != nil {
		return "", err
	}
	w, err := CurrentBudgetWindows(ctx, tx, time.Now())
	if err != nil {
		return "", err
	}
	for _, o := range owners {
		bound := i.CostBound
		if o.Basis == BasisCharge {
			bound = i.ChargeBound
		}
		if _, err = tx.Exec(ctx, `INSERT INTO olp.execution_debit_owners(dispatch_id,owner_id,owner_kind,basis,bound,daily_window_id,weekly_window_id,monthly_window_id) VALUES($1,$2,$3,$4,$5::numeric,$6,$7,$8)`, i.ID, o.ID, o.Kind, o.Basis, bound, w.DailyID, w.WeeklyID, w.MonthlyID); err != nil {
			return "", err
		}
	}
	if err = bumpDebitOwners(ctx, tx, owners); err != nil {
		return "", err
	}
	return DebitPrepared, nil
}

func debitOwnersTx(ctx context.Context, tx pgx.Tx, id string) ([]DebitOwner, error) {
	rows, err := tx.Query(ctx, `SELECT owner_id::text,owner_kind,basis FROM olp.execution_debit_owners WHERE dispatch_id=$1 ORDER BY owner_id,basis`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []DebitOwner
	for rows.Next() {
		var o DebitOwner
		if err = rows.Scan(&o.ID, &o.Kind, &o.Basis); err != nil {
			return nil, err
		}
		owners = append(owners, o)
	}
	return owners, rows.Err()
}

// Admit projects the committed intent and verifies it fits beside ordinary
// leases and every other debit. Requests must cover every recorded boundary,
// even uncapped ones; live policy construction remains with its existing owner.
func (s *DebitStore) Admit(ctx context.Context, id, claim string, requests []Request) error {
	if s.Limiter == nil {
		return ErrDebitAuthority
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state DebitState
	if err = tx.QueryRow(ctx, `SELECT state FROM olp.execution_debits WHERE id=$1 AND claim_id=$2 AND prepared_until>clock_timestamp() FOR UPDATE`, id, claim).Scan(&state); err != nil {
		return ErrDebitState
	}
	if state != DebitPrepared {
		return ErrDebitState
	}
	owners, err := debitOwnersTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(requests)*2 != len(owners) {
		return ErrDebitConflict
	}
	if err = lockDebitOwners(ctx, tx, owners); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range requests {
		key := r.CostOwnerID
		if seen[key] || !slices.ContainsFunc(owners, func(o DebitOwner) bool { return o.ID == r.CostOwnerID && o.Basis == r.CostBasis.normalized() }) {
			return ErrDebitConflict
		}
		seen[key] = true
		if r.HasCostBudget() {
			if err = s.syncOwnerTx(ctx, tx, r.CostOwnerID); err != nil {
				return err
			}
			if err = s.Limiter.verifyDurableCost(ctx, r); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='ready' WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkDispatched is the one-way fence immediately BEFORE remote IO. An
// ambiguous commit is not permission to send. The caller must reauthorize live
// key/resource authority first and must never retry this transition to resend.
func (s *DebitStore) MarkDispatched(ctx context.Context, id, claim string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = MarkDispatchedTx(ctx, tx, id, claim); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func MarkDispatchedTx(ctx context.Context, tx pgx.Tx, id, claim string) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.execution_debits d SET state='dispatched',dispatched_at=clock_timestamp()
 WHERE d.id=$1 AND d.claim_id=$2 AND d.state='ready' AND d.prepared_until>clock_timestamp()
 AND EXISTS(SELECT 1 FROM olp.api_keys k WHERE k.id=d.api_key_id AND k.project_id IS NOT DISTINCT FROM d.project_id AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()))`, id, claim)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrDebitState
	}
	return nil
}

func (s *DebitStore) HasUnknownDebit(ctx context.Context, apiKeyID string, maxDispatch time.Duration) (bool, error) {
	if !canonicalID(apiKeyID) {
		return false, ErrDebitConflict
	}
	if maxDispatch <= 0 {
		maxDispatch = time.Hour
	}
	var held bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.execution_debits d WHERE d.api_key_id=$1 AND (d.state='outcome_unknown' OR (d.state='dispatched' AND d.dispatched_at<clock_timestamp()-make_interval(secs=>$2))) AND d.closed_at IS NULL AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_results r WHERE r.dispatch_id=d.id AND r.final AND EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.event_id=r.event_id) AND NOT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.event_id=r.event_id AND NOT f.usage_complete)))`, apiKeyID, maxDispatch.Seconds()).Scan(&held)
	return held, err
}

func (s *DebitStore) MarkUnknown(ctx context.Context, id, claim string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE olp.execution_debits SET state='outcome_unknown' WHERE id=$1 AND claim_id=$2 AND state IN ('dispatched','outcome_unknown')`, id, claim)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrDebitState
	}
	return err
}

// ReleaseUndispatched is a proof by state, not a provider cancellation guess.
// Once dispatched_at exists, even a worker or a revoked caller cannot refund.
func (s *DebitStore) ReleaseUndispatched(ctx context.Context, id, claim string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state DebitState
	if err = tx.QueryRow(ctx, `SELECT state FROM olp.execution_debits WHERE id=$1 AND claim_id=$2 FOR UPDATE`, id, claim).Scan(&state); err != nil {
		return ErrDebitState
	}
	if state == DebitReleased {
		return tx.Commit(ctx)
	}
	if state != DebitPrepared && state != DebitReady {
		return ErrDebitState
	}
	owners, err := debitOwnersTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err = lockDebitOwners(ctx, tx, owners); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='released',closed_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return err
	}
	if err = bumpDebitOwners(ctx, tx, owners); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DebitResult is content-free evidence already checked by the lifecycle owner.
// ResultDigest identifies one result independently of the evidence digest.
type DebitResult struct {
	DispatchID                   string
	ResultDigest, EvidenceDigest []byte
	EventID                      string
	Cost, Charge                 string
	Final, Complete              bool
	// ResolvesResultDigests links later verified evidence to incomplete receipts.
	// Its event contains only newly observed accounting deltas; prior charges
	// are never replayed. Lifecycle owners verify native evidence before calling.
	ResolvesResultDigests [][]byte
}

// DebitResultReplayTx recognizes immutable settlement evidence independently
// of ordinary usage retention. The caller holds the dispatch row lock.
func DebitResultReplayTx(ctx context.Context, tx pgx.Tx, r DebitResult) (bool, error) {
	if !canonicalID(r.DispatchID) || !canonicalID(r.EventID) || len(r.ResultDigest) != 32 || len(r.EvidenceDigest) != 32 || !validDebitAmount(r.Cost) || !validDebitAmount(r.Charge) {
		return false, ErrDebitConflict
	}
	resolutionDigest, err := debitResolutionDigest(r.ResolvesResultDigests)
	if err != nil || (!r.Complete && len(r.ResolvesResultDigests) > 0) {
		return false, ErrDebitConflict
	}
	var evidence, resolutions []byte
	var event, cost, charge string
	var final, complete bool
	err = tx.QueryRow(ctx, `SELECT evidence_digest,resolution_digest,event_id::text,cost::text,charge::text,final,complete FROM olp.execution_debit_results WHERE dispatch_id=$1 AND result_digest=$2`, r.DispatchID, r.ResultDigest).Scan(&evidence, &resolutions, &event, &cost, &charge, &final, &complete)
	if err == nil {
		if !debitResultMatches(r, resolutionDigest, evidence, resolutions, event, cost, charge, final, complete) {
			return false, ErrDebitConflict
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return false, nil
}

func debitResultMatches(r DebitResult, resolutionDigest, evidence, resolutions []byte, event, cost, charge string, final, complete bool) bool {
	return bytes.Equal(evidence, r.EvidenceDigest) && bytes.Equal(resolutions, resolutionDigest) && event == r.EventID && equalDebitAmount(cost, r.Cost) && equalDebitAmount(charge, r.Charge) && final == r.Final && complete == r.Complete
}

type debitSettleHead struct {
	state          DebitState
	kind           string
	costBound      string
	chargeBound    string
	observedCost   string
	observedCharge string
}

func beginDebitSettleTx(ctx context.Context, tx pgx.Tx, r DebitResult) (debitSettleHead, bool, error) {
	var head debitSettleHead
	if err := tx.QueryRow(ctx, `SELECT state,kind,cost_bound::text,charge_bound::text,observed_cost::text,observed_charge::text FROM olp.execution_debits WHERE id=$1 FOR UPDATE`, r.DispatchID).Scan(&head.state, &head.kind, &head.costBound, &head.chargeBound, &head.observedCost, &head.observedCharge); err != nil {
		return head, false, err
	}
	duplicate, err := DebitResultReplayTx(ctx, tx, r)
	return head, duplicate, err
}

func appendDebitResultTx(ctx context.Context, tx pgx.Tx, r DebitResult, owners []DebitOwner, resolutionDigest []byte) error {
	if err := lockDebitOwners(ctx, tx, owners); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_results(dispatch_id,result_digest,evidence_digest,event_id,cost,charge,final,complete,resolution_digest) VALUES($1,$2,$3,$4,$5::numeric,$6::numeric,$7,$8,$9)`, r.DispatchID, r.ResultDigest, r.EvidenceDigest, r.EventID, r.Cost, r.Charge, r.Final, r.Complete, resolutionDigest)
	return err
}

// SettleDebitTx shares the usage persistence transaction. Accounting contains
// the balances returned by that event's persistence, covering every owner.
func SettleDebitTx(ctx context.Context, tx pgx.Tx, r DebitResult, accounting []CostSnapshot) error {
	head, duplicate, err := beginDebitSettleTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	resolutionDigest, _ := debitResolutionDigest(r.ResolvesResultDigests)
	if head.state != DebitDispatched && head.state != DebitUnknown {
		return ErrDebitState
	}
	var persisted bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT count(*)>0 AND sum(COALESCE(f.estimated_cost,0))=$3::numeric AND sum(COALESCE(f.charge,0))=$4::numeric AND bool_and(f.charge_status='not_billable' OR f.currency=d.currency OR (f.estimated_cost=0 AND f.charge=0) OR (NOT $5 AND f.currency IS NULL)) AND bool_and((d.affinity->>'pricing_revision_id' IS NULL OR f.pricing_revision_id::text=d.affinity->>'pricing_revision_id') AND (d.affinity->>'rate_card_revision_id' IS NULL OR f.rate_card_revision_id::text=d.affinity->>'rate_card_revision_id')) AND (NOT $5 OR bool_and(f.charge_status='not_billable' OR (f.usage_complete AND NOT f.unpriced AND f.estimated_cost IS NOT NULL AND f.charge IS NOT NULL))) FROM olp.attempt_usage_facts f JOIN olp.execution_debits d ON d.id=$2 WHERE f.event_id=$1 AND f.api_key_id=d.api_key_id),false)`, r.EventID, r.DispatchID, r.Cost, r.Charge, r.Complete).Scan(&persisted); err != nil {
		return err
	}
	if !persisted {
		return ErrDebitState
	}
	owners, err := debitOwnersTx(ctx, tx, r.DispatchID)
	if err != nil {
		return err
	}
	if !debitOwnersAccounted(owners, accounting) {
		return ErrDebitState
	}
	if err = appendDebitResultTx(ctx, tx, r, owners, resolutionDigest); err != nil {
		return err
	}
	for _, resolved := range r.ResolvesResultDigests {
		tag, err := tx.Exec(ctx, `INSERT INTO olp.execution_debit_result_resolutions(dispatch_id,resolved_digest,evidence_result_digest) SELECT $1,$2,$3 WHERE $2::bytea<>$3::bytea AND EXISTS(SELECT 1 FROM olp.execution_debit_results WHERE dispatch_id=$1 AND result_digest=$2 AND NOT complete) AND NOT EXISTS(SELECT 1 FROM olp.execution_debit_result_resolutions WHERE dispatch_id=$1 AND resolved_digest=$2)`, r.DispatchID, resolved, r.ResultDigest)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrDebitConflict
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
	return bumpDebitOwners(ctx, tx, owners)
}

func debitOwnersAccounted(owners []DebitOwner, accounting []CostSnapshot) bool {
	for _, owner := range owners {
		basis := owner.Basis
		// Supply budgets measure provider cost under either policy basis.
		if owner.Kind == "supply" {
			basis = BasisCost
		}
		if !slices.ContainsFunc(accounting, func(s CostSnapshot) bool {
			return s.CostOwnerID == owner.ID && s.Basis.normalized() == basis
		}) {
			return false
		}
	}
	return true
}

func debitResolutionDigest(values [][]byte) ([]byte, error) {
	if len(values) > 128 {
		return nil, ErrDebitConflict
	}
	normalized := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) != 32 {
			return nil, ErrDebitConflict
		}
		normalized = append(normalized, hex.EncodeToString(v))
	}
	slices.Sort(normalized)
	for i := 1; i < len(normalized); i++ {
		if normalized[i] == normalized[i-1] {
			return nil, ErrDebitConflict
		}
	}
	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func equalDebitAmount(a, b string) bool {
	// PostgreSQL numeric returns a fixed scale; compare canonical JSON-independent
	// decimal values, not floating point or presentation spelling.
	return trimDecimal(a) == trimDecimal(b)
}
func trimDecimal(v string) string {
	// Inputs have already passed validDecimal or came from constrained numeric.
	for len(v) > 1 && v[0] == '0' && v[1] != '.' {
		v = v[1:]
	}
	if bytes.ContainsRune([]byte(v), '.') {
		for len(v) > 0 && v[len(v)-1] == '0' {
			v = v[:len(v)-1]
		}
		if len(v) > 0 && v[len(v)-1] == '.' {
			v = v[:len(v)-1]
		}
	}
	return v
}

// RecoverPrepared has no dispatch path. The row lock fences a live gateway:
// either its ready->dispatched transition wins, or recovery proves no send was
// authorized and closes the debit. Unknown/dispatched work is never reclaimed.
func (s *DebitStore) RecoverPrepared(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 256 {
		return 0, fmt.Errorf("invalid debit recovery limit")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id::text FROM olp.execution_debits WHERE state IN ('prepared','ready') AND prepared_until<clock_timestamp() ORDER BY prepared_until,id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
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
	var all []DebitOwner
	for _, id := range ids {
		owners, e := debitOwnersTx(ctx, tx, id)
		if e != nil {
			return 0, e
		}
		all = append(all, owners...)
	}
	slices.SortFunc(all, func(a, b DebitOwner) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	if err = lockDebitOwners(ctx, tx, all); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if _, err = tx.Exec(ctx, `UPDATE olp.execution_debits SET state='released',closed_at=clock_timestamp() WHERE id=ANY($1::uuid[])`, ids); err != nil {
			return 0, err
		}
		if err = bumpDebitOwners(ctx, tx, all); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

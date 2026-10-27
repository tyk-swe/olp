//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestExecutionDebitReconciliationContinuesAcrossBatchesAndFailures(t *testing.T) {
	f, store, _ := debitFixture(t)
	valkey, prefix, _ := acctKeyspace(t)
	store.Limiter, _ = limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	acctExec(t, f.Pool, `INSERT INTO olp.execution_debit_versions(owner_id,version)
 SELECT ('00000000-0000-4000-8000-'||lpad(i::text,12,'0'))::uuid,1 FROM generate_series(1,128) i`)
	acctExec(t, f.Pool, `INSERT INTO olp.execution_debit_versions(owner_id,version) VALUES($1,1)`, f.Key)
	projection := prefix + "{" + strings.ReplaceAll(f.Key, "-", "") + "}:cost:durable"
	exists := func() bool {
		t.Helper()
		value, err := valkey.Do(t.Context(), "EXISTS", projection)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(value) == "1"
	}
	if err := store.ReconcileDebits(t.Context()); err != nil {
		t.Fatal(err)
	}
	if exists() {
		t.Fatal("reconciliation did not stop at its bounded owner batch")
	}
	if err := store.ReconcileDebits(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Fatal("next pass restarted instead of reaching the later owner")
	}
	if _, err := valkey.Do(t.Context(), "DEL", projection); err != nil {
		t.Fatal(err)
	}
	lock, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(t.Context())
	if _, err = lock.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('olp:execution-debit:'||$1,0))`, "00000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	pass, cancel := context.WithTimeout(t.Context(), time.Second)
	err = store.ReconcileDebits(pass)
	cancel()
	if err == nil {
		t.Fatal("blocked owner did not fail the timed reconciliation pass")
	}
	if err = lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = store.ReconcileDebits(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Fatal("failed owner starved later owners on the next pass")
	}
}

func TestExecutionDebitWorkerHealthIncludesFailures(t *testing.T) {
	pool := acctPool(t)
	if err := usage.CheckpointTask(t.Context(), pool, usage.TaskExecutionDebits, usage.OutcomeFailure, false); err != nil {
		t.Fatal(err)
	}
	health, err := observability.ReadWorkerTaskHealth(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range health.Tasks {
		if task.Task == string(usage.TaskExecutionDebits) {
			if task.State != observability.TaskStateStale || task.FailuresTotal != 1 {
				t.Fatalf("debit failure was hidden from worker health: %+v", task)
			}
			return
		}
	}
	t.Fatal("debit reconciliation was absent from worker health")
}

func debitFixture(t *testing.T) (acctFixture, *limits.DebitStore, string) {
	t.Helper()
	f := acctSeed(t, acctPool(t))
	project := acctID(t)
	acctExec(t, f.Pool, `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,'Debit project',$2,$3)`, project, acctID(t), f.User)
	acctExec(t, f.Pool, `UPDATE olp.api_keys SET project_id=$2 WHERE id=$1`, f.Key, project)
	acctExec(t, f.Pool, `INSERT INTO olp.pricing_currency(singleton,currency) VALUES(true,'USD')`)
	acctPricing(t, f, 1, time.Now().Add(-time.Hour), acctPrice{Kind: "openai", Model: "debit-model", Operation: "generation", Input: acctPtr("1"), Output: acctPtr("1")})
	card := acctID(t)
	acctExec(t, f.Pool, `INSERT INTO olp.rate_cards(id,project_id,created_by) VALUES($1,$2,$3)`, card, project, f.User)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = usage.CreateCardRevision(t.Context(), tx, f.User, card, "1.5", time.Now().Add(-time.Hour), nil, "USD", nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err := limits.CurrentBudgetWindows(t.Context(), f.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for kind, id := range map[string]int64{"day": w.DailyID, "week": w.WeeklyID, "month": w.MonthlyID} {
		acctExec(t, f.Pool, `INSERT INTO olp.api_key_cost_windows(api_key_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete) VALUES($1,$2,$3,0,0,true)`, f.Key, kind, id)
	}
	return f, &limits.DebitStore{Pool: f.Pool}, project
}

func debitIntent(t *testing.T, f acctFixture, project string) limits.DebitIntent {
	t.Helper()
	return limits.DebitIntent{ID: acctID(t), APIKeyID: f.Key, ProjectID: project, ClaimID: acctID(t), Kind: "api", Affinity: limits.ExecutionAffinity{ReleaseID: acctID(t), RevisionID: acctID(t), ProviderID: f.Provider, ProviderRevisionID: f.Revision}, Currency: "USD", CostBound: "2", ChargeBound: "3", PreparedUntil: time.Now().Add(time.Minute)}
}

func prepareDebit(t *testing.T, f acctFixture, s *limits.DebitStore, i limits.DebitIntent) {
	t.Helper()
	owners := []limits.DebitOwner{{ID: f.Key, Kind: "key"}, {ID: f.Provider, Kind: "supply"}}
	if state, err := s.Prepare(t.Context(), i, owners); err != nil || state != limits.DebitPrepared {
		t.Fatalf("prepare: %s %v", state, err)
	}
}

func TestExecutionDebitOrdinaryUTCSnapshot(t *testing.T) {
	f, store, _ := debitFixture(t)
	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	limiter.UseDurableDebits(f.Pool)
	store.Limiter = limiter
	if err = store.SyncOwner(t.Context(), f.Key); err != nil {
		t.Fatal(err)
	}
	cap, requestID := "1", acctID(t)
	if _, err = limiter.Reserve(t.Context(), limits.Request{LookupID: strings.ReplaceAll(f.Key, "-", ""), CostOwnerID: f.Key, DailyCostLimit: &cap, CostEstimate: "0.4", RequestID: requestID, LeaseTTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	w := limits.BudgetWindows(time.Now())
	snapshot := limits.CostSnapshot{
		CostOwnerID: f.Key, RequestID: requestID,
		DailyWindowID: w.DailyID, DailyStart: w.DailyStart, DailyEnd: w.DailyEnd, DailyAccrued: "0.75",
		MonthlyWindowID: w.MonthlyID, MonthlyStart: w.MonthlyStart, MonthlyEnd: w.MonthlyEnd, MonthlyAccrued: "0.75",
		WeeklyWindowID: w.WeeklyID, WeeklyStart: w.WeeklyStart, WeeklyEnd: w.WeeklyEnd, WeeklyAccrued: "0.75",
	}
	day, month, err := limiter.ApplyCostSnapshot(t.Context(), snapshot)
	if err != nil || !day || !month {
		t.Fatalf("ordinary UTC spend did not update durable-initialized balances: day=%v month=%v error=%v", day, month, err)
	}
	if reserved, err := limiter.Reserved(t.Context(), f.Key); err != nil || reserved != "0" {
		t.Fatalf("settled reservation remains: %s %v", reserved, err)
	}
	_, err = limiter.Reserve(t.Context(), limits.Request{LookupID: strings.ReplaceAll(f.Key, "-", ""), CostOwnerID: f.Key, DailyCostLimit: &cap, CostEstimate: "0.3", RequestID: acctID(t), LeaseTTL: time.Minute})
	var exceeded *limits.ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("ordinary spend was omitted from admission: %v", err)
	}
}

func TestExecutionDebitFencesRecoveryAndDurableProjection(t *testing.T) {
	f, s, project := debitFixture(t)
	valkey, prefix, _ := acctKeyspace(t)
	namespace := strings.TrimSuffix(prefix, ":")
	limiter, err := limits.New(valkey, namespace)
	if err != nil {
		t.Fatal(err)
	}
	limiter.UseDurableDebits(f.Pool)
	s.Limiter = limiter
	peerValkey, peerPrefix, _ := acctKeyspace(t)
	peerLimiter, err := limits.New(peerValkey, strings.TrimSuffix(peerPrefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	peerLimiter.UseDurableDebits(f.Pool)
	peerStore := &limits.DebitStore{Pool: f.Pool, Limiter: peerLimiter}
	if err = peerStore.SyncOwner(t.Context(), f.Key); err != nil {
		t.Fatal(err)
	}
	i := debitIntent(t, f, project)
	prepareDebit(t, f, s, i)
	cap := "2"
	requests := []limits.Request{{CostOwnerID: f.Key, DailyCostLimit: &cap}, {CostOwnerID: f.Provider}}
	if err = s.Admit(t.Context(), i.ID, i.ClaimID, requests[:1]); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("missing owner accepted: %v", err)
	}
	if err = s.Admit(t.Context(), i.ID, i.ClaimID, requests); err != nil {
		t.Fatalf("exact-cap admission: %v", err)
	}
	// A region with a warm zero projection must see the other region's new
	// generation during its existing pending-accounting check.
	if err = peerStore.CheckOwnerAccounting(t.Context(), f.Key); err != nil {
		t.Fatal(err)
	}
	_, err = peerLimiter.Reserve(t.Context(), limits.Request{LookupID: strings.ReplaceAll(f.Key, "-", ""), CostOwnerID: f.Key, DailyCostLimit: &cap, CostEstimate: "0.1", RequestID: acctID(t), LeaseTTL: time.Minute})
	var exceeded *limits.ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("stale regional projection admitted spend: %v", err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); outcomes <- s.MarkDispatched(t.Context(), i.ID, i.ClaimID) }()
	}
	wg.Wait()
	close(outcomes)
	winners := 0
	for outcome := range outcomes {
		if outcome == nil {
			winners++
		} else if !errors.Is(outcome, limits.ErrDebitState) {
			t.Fatal(outcome)
		}
	}
	if winners != 1 {
		t.Fatalf("dispatch fences granted %d sends", winners)
	}
	if err = s.MarkUnknown(t.Context(), i.ID, i.ClaimID); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseUndispatched(t.Context(), i.ID, i.ClaimID); !errors.Is(err, limits.ErrDebitState) {
		t.Fatalf("ambiguous dispatch refunded: %v", err)
	}
	if err = s.MarkDispatched(t.Context(), i.ID, i.ClaimID); !errors.Is(err, limits.ErrDebitState) {
		t.Fatalf("ambiguous dispatch replayed: %v", err)
	}
	checkProjection := func() {
		t.Helper()
		raw, err := valkey.Do(t.Context(), "KEYS", prefix+"*:cost:durable")
		if err != nil {
			t.Fatal(err)
		}
		keys, ok := raw.([]any)
		if !ok || len(keys) != 1 {
			t.Fatalf("projection keys: %v", raw)
		}
		key := fmt.Sprint(keys[0])
		amount, err := valkey.Do(t.Context(), "HGET", key, "amount")
		if err != nil {
			t.Fatal(err)
		}
		if d, err := decimal.NewFromString(fmt.Sprint(amount)); err != nil || !d.Equal(decimal.NewFromInt(2)) {
			t.Fatalf("lost unknown exposure: %v %v", amount, err)
		}
		ttl, err := valkey.Do(t.Context(), "PTTL", key)
		if err != nil || fmt.Sprint(ttl) != "-1" {
			t.Fatalf("durable exposure has a lease TTL: %v %v", ttl, err)
		}
		if _, err = valkey.Do(t.Context(), "DEL", key); err != nil {
			t.Fatal(err)
		}
	}
	checkProjection()
	replacement := &limits.DebitStore{Pool: f.Pool, Limiter: limiter}
	if err = replacement.SyncOwner(t.Context(), f.Key); err != nil {
		t.Fatal(err)
	}
	checkProjection()
	// The only automatically refundable rows are fenced, undispatched intents.
	abandoned := debitIntent(t, f, project)
	abandoned.PreparedUntil = time.Now().Add(2 * time.Second)
	prepareDebit(t, f, s, abandoned)
	time.Sleep(2100 * time.Millisecond)
	if n, err := replacement.RecoverPrepared(t.Context(), 256); err != nil || n != 1 {
		t.Fatalf("prepared recovery: %d %v", n, err)
	}
	var state string
	if err = f.Pool.QueryRow(t.Context(), `SELECT state FROM olp.execution_debits WHERE id=$1`, i.ID).Scan(&state); err != nil || state != "outcome_unknown" {
		t.Fatalf("recovery released unknown work: %s %v", state, err)
	}
	var bases int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(DISTINCT basis) FROM olp.execution_debit_owners WHERE dispatch_id=$1`, i.ID).Scan(&bases); err != nil || bases != 2 {
		t.Fatalf("policy basis change loses exposure: %d %v", bases, err)
	}
}

func TestExecutionDebitSettlementIsAtomicAndIdempotent(t *testing.T) {
	f, s, project := debitFixture(t)
	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	s.Limiter = limiter
	i := debitIntent(t, f, project)
	i.CostBound = "0.5"
	i.ChargeBound = "0.75"
	prepareDebit(t, f, s, i)
	if err = s.Admit(t.Context(), i.ID, i.ClaimID, []limits.Request{{CostOwnerID: f.Key}, {CostOwnerID: f.Provider}}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkDispatched(t.Context(), i.ID, i.ClaimID); err != nil {
		t.Fatal(err)
	}
	event := acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{acctAttempt(t, f.Provider, 1, "debit-model", 200, acctObserved(1_000_000, 0, nil, nil))}})
	event.RuntimeGenerationID = i.Affinity.ReleaseID
	event.Attempts[0].Execution = &usage.ExecutionReference{DispatchID: i.ID}
	event.Attempts[0].Routing = &usage.Routing{ProviderRevisionID: f.Revision}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	result := limits.DebitResult{DispatchID: i.ID, ResultDigest: bytes.Repeat([]byte{1}, 32), EvidenceDigest: bytes.Repeat([]byte{2}, 32), EventID: event.EventID, Cost: "1", Charge: "1.5", Final: true, Complete: true}
	persist := func(result limits.DebitResult, commit bool) error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if _, err = usage.PersistDebitResultTx(t.Context(), tx, event, payload, result); err != nil {
			return err
		}
		if commit {
			return tx.Commit(t.Context())
		}
		return nil
	}
	// Ordinary key accounting alone cannot release an unaccounted supply hold.
	checkTx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer checkTx.Rollback(t.Context())
	ordinary, err := usage.PersistEventTx(t.Context(), checkTx, event, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = limits.SettleDebitTx(t.Context(), checkTx, result, ordinary.CostSnapshots); !errors.Is(err, limits.ErrDebitState) {
		t.Fatalf("settlement released unaccounted supply exposure: %v", err)
	}
	if err = checkTx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = persist(result, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE event_id=$1`, event.EventID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback left a charge: %d %v", count, err)
	}
	if err = persist(result, true); err != nil {
		t.Fatal(err)
	}
	if err = persist(result, true); err != nil {
		t.Fatalf("duplicate evidence: %v", err)
	}
	conflicting := result
	conflicting.EvidenceDigest = bytes.Repeat([]byte{3}, 32)
	if err = persist(conflicting, true); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("conflicting result accepted: %v", err)
	}
	var state string
	var violation bool
	var cost, charge string
	if err = f.Pool.QueryRow(t.Context(), `SELECT state,bound_violation,observed_cost::text,observed_charge::text FROM olp.execution_debits WHERE id=$1`, i.ID).Scan(&state, &violation, &cost, &charge); err != nil {
		t.Fatal(err)
	}
	if state != "settled" || !violation || !decimal.RequireFromString(cost).Equal(decimal.NewFromInt(1)) || !decimal.RequireFromString(charge).Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("observed overrun was lost or billed twice: %s %v %s/%s", state, violation, cost, charge)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_results WHERE dispatch_id=$1`, i.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate settlement receipt: %d %v", count, err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.supply_cost_windows WHERE owner_id=$1 AND accrued=1`, f.Provider).Scan(&count); err != nil || count != 3 {
		t.Fatalf("uncapped supply owner was not accounted exactly once: %d %v", count, err)
	}
	// Retain debit evidence while ordinary receipts and facts expire.
	acctExec(t, f.Pool, `DELETE FROM olp.attempt_usage_facts WHERE event_id=$1`, event.EventID)
	acctExec(t, f.Pool, `DELETE FROM olp.request_metadata_event_receipts WHERE event_id=$1`, event.EventID)
	shift := -time.Duration(usage.ReplayHorizonDays+1) * 24 * time.Hour
	event.ObservedAt = event.ObservedAt.Add(shift)
	event.RequestStartedAt = event.RequestStartedAt.Add(shift)
	event.RequestCompletedAt = event.RequestCompletedAt.Add(shift)
	for n := range event.Attempts {
		event.Attempts[n].StartedAt = event.Attempts[n].StartedAt.Add(shift)
		event.Attempts[n].CompletedAt = event.Attempts[n].CompletedAt.Add(shift)
	}
	if _, err = usage.Validate(event); err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(event)
	var gaps int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.request_metadata_ingestion_gaps`).Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if err = persist(result, true); err != nil {
		t.Fatalf("retained settlement replay: %v", err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.request_metadata_ingestion_gaps`).Scan(&count); err != nil || count != gaps {
		t.Fatalf("settlement replay created an ingestion gap: %d -> %d %v", gaps, count, err)
	}
}

func TestExecutionDebitDefinitiveRejectionSettlesAsUnbilled(t *testing.T) {
	f, s, project := debitFixture(t)
	valkey, prefix, _ := acctKeyspace(t)
	s.Limiter, _ = limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	i := debitIntent(t, f, project)
	prepareDebit(t, f, s, i)
	if err := s.Admit(t.Context(), i.ID, i.ClaimID, []limits.Request{{CostOwnerID: f.Key}, {CostOwnerID: f.Provider}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(t.Context(), i.ID, i.ClaimID); err != nil {
		t.Fatal(err)
	}
	event := acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{acctAttempt(t, f.Provider, 1, "debit-model", 400, acctSettled())}})
	event.RuntimeGenerationID = i.Affinity.ReleaseID
	event.Attempts[0].Routing = &usage.Routing{ProviderRevisionID: i.Affinity.ProviderRevisionID}
	event.Attempts[0].Execution = &usage.ExecutionReference{DispatchID: i.ID}
	payload, err := usage.Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = usage.PersistDebitEventTx(t.Context(), tx, event, payload); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var state string
	var closed bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT state,closed_at IS NOT NULL FROM olp.execution_debits WHERE id=$1`, i.ID).Scan(&state, &closed); err != nil || state != "settled" || !closed {
		t.Fatalf("definitive rejection did not settle its debit as unbilled: state=%s closed=%v err=%v", state, closed, err)
	}
	if held, err := s.HasUnknownDebit(t.Context(), f.Key); err != nil || held {
		t.Fatalf("settled rejection still holds session exposure: held=%v err=%v", held, err)
	}
}

func TestExecutionDebitPrepareSupportsUnsetPricingCurrency(t *testing.T) {
	f := acctSeed(t, acctPool(t))
	project := acctID(t)
	acctExec(t, f.Pool, `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,'Unpriced project',$2,$3)`, project, acctID(t), f.User)
	acctExec(t, f.Pool, `UPDATE olp.api_keys SET project_id=$2 WHERE id=$1`, f.Key, project)
	s := &limits.DebitStore{Pool: f.Pool}
	i := debitIntent(t, f, project)
	owners := []limits.DebitOwner{{ID: f.Key, Kind: "key"}}
	if state, err := s.Prepare(t.Context(), i, owners); err != nil || state != limits.DebitPrepared {
		t.Fatalf("unpriced installation cannot prepare a session debit: %s %v", state, err)
	}
	foreign := debitIntent(t, f, project)
	foreign.Currency = "EUR"
	if _, err := s.Prepare(t.Context(), foreign, owners); !errors.Is(err, limits.ErrDebitAuthority) {
		t.Fatalf("foreign currency accepted without pricing: %v", err)
	}
}

func TestExecutionDebitNonAcceptanceRequiresImmutableProof(t *testing.T) {
	f, s, project := debitFixture(t)
	valkey, prefix, _ := acctKeyspace(t)
	s.Limiter, _ = limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	i := debitIntent(t, f, project)
	prepareDebit(t, f, s, i)
	if err := s.Admit(t.Context(), i.ID, i.ClaimID, []limits.Request{{CostOwnerID: f.Key}, {CostOwnerID: f.Provider}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(t.Context(), i.ID, i.ClaimID); err != nil {
		t.Fatal(err)
	}
	event := acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{acctAttempt(t, f.Provider, 1, "debit-model", 500, acctUncertain())}})
	event.RuntimeGenerationID = i.Affinity.ReleaseID
	event.Attempts[0].Routing = &usage.Routing{ProviderRevisionID: i.Affinity.ProviderRevisionID}
	event.Attempts[0].Execution = &usage.ExecutionReference{DispatchID: i.ID}
	payload, _ := json.Marshal(event)
	receipt := limits.DebitResult{DispatchID: i.ID, EventID: event.EventID, ResultDigest: bytes.Repeat([]byte{1}, 32), EvidenceDigest: bytes.Repeat([]byte{2}, 32), Cost: "0", Charge: "0", Final: true}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = usage.PersistDebitResultTx(t.Context(), tx, event, payload, receipt); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	proof := limits.NonAcceptanceProof{DispatchID: i.ID, ActorID: f.User, EvidenceDigest: bytes.Repeat([]byte{1}, 32)}
	resolve := func(p limits.NonAcceptanceProof) error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err = limits.ResolveNotAcceptedTx(t.Context(), tx, p); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	if err := resolve(proof); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_results WHERE dispatch_id=$1`, i.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("resolution erased uncertain evidence: %d %v", retained, err)
	}
	if err := resolve(proof); err != nil {
		t.Fatal(err)
	}
	proof.EvidenceDigest = bytes.Repeat([]byte{2}, 32)
	if err := resolve(proof); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("conflicting reconciliation proof accepted: %v", err)
	}
	if err := s.MarkDispatched(t.Context(), i.ID, i.ClaimID); !errors.Is(err, limits.ErrDebitState) {
		t.Fatalf("reconciled work replayed: %v", err)
	}
}

func TestExecutionDebitPreservesGlobalNamespace(t *testing.T) {
	f, s, project := debitFixture(t)
	i := debitIntent(t, f, project)
	i.ProjectID = ""
	owners := []limits.DebitOwner{{ID: f.Key, Kind: "key"}}
	if _, err := s.Prepare(t.Context(), i, owners); !errors.Is(err, limits.ErrDebitAuthority) {
		t.Fatalf("project key escaped to global namespace: %v", err)
	}
	acctExec(t, f.Pool, `UPDATE olp.api_keys SET project_id=NULL WHERE id=$1`, f.Key)
	if _, err := s.Prepare(t.Context(), i, owners); err != nil {
		t.Fatal(err)
	}
	var global bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT project_id IS NULL FROM olp.execution_debits WHERE id=$1`, i.ID).Scan(&global); err != nil || !global {
		t.Fatal("global namespace changed", err)
	}
}

func TestExecutionDebitFinalResultCannotEraseEarlierUncertainty(t *testing.T) {
	f, s, project := debitFixture(t)
	client, prefix, _ := acctKeyspace(t)
	var err error
	s.Limiter, err = limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	i := debitIntent(t, f, project)
	prepareDebit(t, f, s, i)
	if err = s.Admit(t.Context(), i.ID, i.ClaimID, []limits.Request{{CostOwnerID: f.Key}, {CostOwnerID: f.Provider}}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkDispatched(t.Context(), i.ID, i.ClaimID); err != nil {
		t.Fatal(err)
	}
	makeEvent := func(evidence *usage.AttemptUsage) *usage.Event {
		e := acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{acctAttempt(t, f.Provider, 1, "debit-model", 200, evidence)}})
		e.RuntimeGenerationID = i.Affinity.ReleaseID
		e.Attempts[0].Execution = &usage.ExecutionReference{DispatchID: i.ID}
		e.Attempts[0].Routing = &usage.Routing{ProviderRevisionID: f.Revision}
		return e
	}
	persist := func(event *usage.Event, result limits.DebitResult) {
		t.Helper()
		raw, err := usage.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		if _, err = usage.PersistDebitResultTx(t.Context(), tx, event, raw, result); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	first := makeEvent(acctUncertain())
	missing := bytes.Repeat([]byte{7}, 32)
	persist(first, limits.DebitResult{DispatchID: i.ID, EventID: first.EventID, ResultDigest: missing, EvidenceDigest: bytes.Repeat([]byte{8}, 32), Cost: "0", Charge: "0"})
	last := makeEvent(acctObserved(1_000_000, 0, nil, nil))
	persist(last, limits.DebitResult{DispatchID: i.ID, EventID: last.EventID, ResultDigest: bytes.Repeat([]byte{9}, 32), EvidenceDigest: bytes.Repeat([]byte{10}, 32), Cost: "1", Charge: "1.5", Final: true, Complete: true})
	var state string
	var closed bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT state,closed_at IS NOT NULL FROM olp.execution_debits WHERE id=$1`, i.ID).Scan(&state, &closed); err != nil || state != "outcome_unknown" || closed {
		t.Fatalf("final result erased earlier uncertainty: %s %v %v", state, closed, err)
	}
	// Later verified coverage contains only new accounting deltas, here zero.
	covered := makeEvent(acctObserved(0, 0, nil, nil))
	persist(covered, limits.DebitResult{DispatchID: i.ID, EventID: covered.EventID, ResultDigest: bytes.Repeat([]byte{11}, 32), EvidenceDigest: bytes.Repeat([]byte{12}, 32), Cost: "0", Charge: "0", Final: true, Complete: true, ResolvesResultDigests: [][]byte{missing}})
	var cost, charge string
	if err = f.Pool.QueryRow(t.Context(), `SELECT state,observed_cost::text,observed_charge::text FROM olp.execution_debits WHERE id=$1`, i.ID).Scan(&state, &cost, &charge); err != nil || state != "settled" || cost != "1.000000000000" || charge != "1.500000000000" {
		t.Fatalf("coverage replayed charges: %s %s %s %v", state, cost, charge, err)
	}
}

//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/retainedworker"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type retainedLedgerFixture struct {
	acctFixture
	store      *resources.Store
	resource   *resources.Resource
	project    string
	credential string
}

func retainedLedger(t *testing.T, operation string, tokenConsuming bool) retainedLedgerFixture {
	t.Helper()
	return retainedLedgerOnPool(t, acctPool(t), operation, tokenConsuming)
}

func retainedLedgerOnPool(t *testing.T, pool *pgxpool.Pool, operation string, tokenConsuming bool) retainedLedgerFixture {
	t.Helper()
	f := acctSeed(t, pool)
	acctExec(t, f.Pool, `UPDATE olp.api_keys SET lookup_id=$2,digest=$3 WHERE id=$1`, f.Key, strings.ReplaceAll(f.Key, "-", "")[:16], make([]byte, 32))
	project, credential, slot, target := acctID(t), acctID(t), acctID(t), acctID(t)
	acctExec(t, f.Pool, `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,'Retained project',$2,$3)`, project, acctID(t), f.User)
	acctExec(t, f.Pool, `UPDATE olp.api_keys SET project_id=$2 WHERE id=$1`, f.Key, project)
	acctExec(t, f.Pool, `INSERT INTO olp.provider_credentials(id,provider_id,version) VALUES($1,$2,1)`, credential, f.Provider)
	routeID, routeRevision := acctID(t), acctID(t)
	slug := "retained-" + f.Key[:8]
	acctExec(t, f.Pool, `INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id) VALUES($1,$6,$2,1,$3,$4,$5)`, routeID, f.User, routeRevision, acctID(t), project, slug)
	targets, _ := json.Marshal([]routing.PublishedTarget{{ID: target, ProviderID: f.Provider, ProviderModel: "retained-model", Weight: 1, TimeoutMS: 1000}})
	operations, _ := json.Marshal([]string{operation, "generation"})
	acctExec(t, f.Pool, `INSERT INTO olp.route_revisions(id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,fidelity) VALUES($1,$2,1,$7,$3,2000,1,$4,$5,$6,'{}','{"mode":"transformed"}')`, routeRevision, routeID, operations, targets, acctID(t), f.User, slug)
	slots, _ := json.Marshal([]routing.RevisionSlot{{Slot: routing.Slot{ID: slot, Enabled: true, Weight: 1, CredentialID: &credential}, Default: true}})
	acctExec(t, f.Pool, `UPDATE olp.provider_revisions SET slots=$2 WHERE id=$1`, f.Revision, slots)
	var revision int
	if err := f.Pool.QueryRow(t.Context(), `SELECT COALESCE(max(revision),0)+1 FROM olp.pricing_revisions`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	price := acctPricing(t, f, revision, time.Now().Add(-time.Hour), acctPrice{Kind: "openai", Model: "retained-model", Operation: operation})
	acctExec(t, f.Pool, `UPDATE olp.prices SET training_per_million=1,storage_per_gib_day=1,cache_storage_per_million_token_hour=1,container_per_session='{"1g":"1"}',per_request=7 WHERE pricing_revision_id=$1`, price)
	attempt := acctAttempt(t, f.Provider, 1, "retained-model", 200, acctUncertain())
	attempt.Routing = &usage.Routing{ProviderRevisionID: f.Revision, CredentialSlotID: &slot, CredentialVersionID: &credential, PricingRevisionID: &price}
	event := acctEvent(t, f, acctEventOptions{Operation: operation, Attempts: []usage.Attempt{attempt}})
	event.Origin = usage.OriginRetainedSettlement
	event.ParentRequestID = new(event.RequestID)
	event.RouteSlug = slug
	kind := map[string]string{"fine_tuning": "fine_tuning_job", "vector_store": "vector_store", "container": "container", "context_cache": "context_cache", "eval": "eval_run"}[operation]
	fields := map[string]any{"upstream_model": "retained-model"}
	if operation == "container" {
		fields["memory_tier"] = "1g"
	}
	metadata, _ := json.Marshal(fields)
	version := retainedapi.Revision
	r := &resources.Resource{Kind: kind, APIKeyID: f.Key, RouteSlug: slug, ProviderID: f.Provider, ProviderRevisionID: f.Revision, RouteRevisionID: routeRevision, TargetID: target, SlotID: slot, CredentialID: &credential, Metadata: metadata, ContractVersion: &version, RetainedAccounting: &resources.RetainedAccounting{Operation: operation, TokenConsuming: tokenConsuming, Templates: map[string]*usage.Event{"retained-model": event}}}
	return retainedLedgerFixture{acctFixture: f, store: resources.New(f.Pool), resource: r, project: project, credential: credential}
}

func (f retainedLedgerFixture) prepare(t *testing.T) *resources.Resource {
	t.Helper()
	r, fresh, err := f.store.PrepareLifecycle(t.Context(), f.resource, f.resource.RetainedAccounting.Operation, "submission-identity", bytes.Repeat([]byte{1}, 32))
	if err != nil || !fresh {
		t.Fatalf("prepare retained settlement: fresh=%v error=%v", fresh, err)
	}
	if err = f.store.DispatchLifecycle(t.Context(), r.UUID); err != nil {
		t.Fatal(err)
	}
	r.UpstreamID, r.State = "provider-resource", "active"
	if err = f.store.CompleteLifecycle(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func retainedTrainingObservation(r *resources.Resource, tokens int64) resources.RetainedObservation {
	now := time.Now().UTC()
	return resources.RetainedObservation{ObservedAt: now, Lines: []resources.RetainedEvidenceLine{{Component: "training", Model: "retained-model", Start: r.CreatedAt.UTC().Add(-time.Second), End: now, Usage: usage.AttemptUsage{TrainingTokens: &tokens}}}}
}

func assertRetainedLedgerCounts(t *testing.T, f retainedLedgerFixture, r *resources.Resource, evidence, coverage, facts int) {
	t.Helper()
	var a, b, c int
	err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM olp.retained_usage_evidence WHERE resource_id=$1),(SELECT count(*) FROM olp.retained_usage_coverage WHERE resource_id=$1),(SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id IN (SELECT event_id FROM olp.retained_usage_coverage WHERE resource_id=$1))`, r.UUID).Scan(&a, &b, &c)
	if err != nil || a != evidence || b != coverage || c != facts {
		t.Fatalf("retained atomic counts evidence=%d coverage=%d facts=%d: %v", a, b, c, err)
	}
}

func TestRetainedSettlementSubmissionAndMutationFencesSurviveRestart(t *testing.T) {
	f := retainedLedger(t, "fine_tuning", true)
	const callers = 8
	type result struct {
		r     *resources.Resource
		fresh bool
		err   error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			candidate := *f.resource
			r, fresh, err := f.store.PrepareLifecycle(t.Context(), &candidate, "fine_tuning", "same-submission", bytes.Repeat([]byte{1}, 32))
			results <- result{r, fresh, err}
		})
	}
	wg.Wait()
	close(results)
	var accepted *resources.Resource
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if accepted != nil && accepted.ID != result.r.ID {
			t.Fatal("concurrent submissions acquired different resources")
		}
		accepted = result.r
		if result.fresh {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("dispatch rights acquired %d times", winners)
	}
	if err := f.store.DispatchLifecycle(t.Context(), accepted.UUID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DispatchLifecycle(t.Context(), accepted.UUID); !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("repeat dispatch rights: %v", err)
	}
	if err := f.store.UnknownLifecycle(t.Context(), accepted.UUID); err != nil {
		t.Fatal(err)
	}
	restarted := resources.New(f.Pool)
	recovered, err := restarted.LookupLifecycle(t.Context(), f.Key, "fine_tuning", "same-submission", bytes.Repeat([]byte{1}, 32))
	if err != nil || recovered.State != "submission_unknown" || recovered.ID != accepted.ID {
		t.Fatalf("unknown acceptance lost across restart: %+v %v", recovered, err)
	}
	if _, err = restarted.LookupLifecycle(t.Context(), f.Key, "fine_tuning", "same-submission", bytes.Repeat([]byte{2}, 32)); !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("conflicting submission reused intent: %v", err)
	}
	mutations := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			mutations <- restarted.PrepareRetainedMutation(t.Context(), accepted, "fine_tuning:patch", "same-mutation", bytes.Repeat([]byte{3}, 32))
		})
	}
	wg.Wait()
	close(mutations)
	winners = 0
	for err := range mutations {
		if err == nil {
			winners++
		} else if !errors.Is(err, resources.ErrSubmissionConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("mutation dispatch rights acquired %d times", winners)
	}
	if err = restarted.UnknownRetainedMutation(t.Context(), accepted.UUID, "same-mutation", "fine_tuning:patch"); err != nil {
		t.Fatal(err)
	}
	if err = resources.New(f.Pool).PrepareRetainedMutation(t.Context(), accepted, "fine_tuning:patch", "same-mutation", bytes.Repeat([]byte{3}, 32)); !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("mutation redispatched after restart: %v", err)
	}
}

func TestRetainedSettlementAtomicEvidenceAcrossSources(t *testing.T) {
	f := retainedLedger(t, "fine_tuning", true)
	r := f.prepare(t)
	observation := retainedTrainingObservation(r, 1_000_000)
	digest := strings.Repeat("a", 64)
	// Simulate a process dying after evidence/facts are written but before its
	// transaction commits. Neither side of the accounting boundary may survive.
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ApplyRetainedEvidenceTx(t.Context(), tx, r.UUID, "automatic", digest, "provider_verified", "", observation, nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertRetainedLedgerCounts(t, f, r, 0, 0, 0)
	if _, err = f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "automatic", digest, "provider_verified", "", observation, nil); err != nil {
		t.Fatal(err)
	}
	assertRetainedLedgerCounts(t, f, r, 1, 1, 1)
	// Equal administrator evidence cannot repeat an automatically observed bill.
	if _, err = f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "administrator", digest, "operator_verified", f.User, observation, nil); err != nil {
		t.Fatal(err)
	}
	assertRetainedLedgerCounts(t, f, r, 2, 1, 1)
	if _, err = resources.New(f.Pool).ApplyRetainedEvidence(t.Context(), r.UUID, "automatic", digest, "provider_verified", "", observation, nil); err != nil {
		t.Fatal(err)
	}
	assertRetainedLedgerCounts(t, f, r, 2, 1, 1)
	conflicting := observation
	conflicting.Lines = append([]resources.RetainedEvidenceLine(nil), observation.Lines...)
	conflicting.Lines[0].End = conflicting.Lines[0].End.Add(time.Microsecond)
	if _, err = f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "overlap", strings.Repeat("b", 64), "operator_verified", f.User, conflicting, nil); !errors.Is(err, resources.ErrEvidenceConflict) {
		t.Fatalf("overlapping interval accepted: %v", err)
	}
	assertRetainedLedgerCounts(t, f, r, 2, 1, 1)
	var cost string
	if err = f.Pool.QueryRow(t.Context(), `SELECT estimated_cost::text FROM olp.attempt_usage_facts WHERE request_id=(SELECT event_id FROM olp.retained_usage_coverage WHERE resource_id=$1)`, r.UUID).Scan(&cost); err != nil || strings.TrimRight(strings.TrimRight(cost, "0"), ".") != "1" {
		t.Fatalf("deferred usage repeated submission fee: %s %v", cost, err)
	}
}

func TestRetainedSettlementLateBudgetsDeletionAndTokenReadiness(t *testing.T) {
	for _, tc := range []struct {
		operation string
		token     bool
	}{{"fine_tuning", true}, {"vector_store", false}} {
		t.Run(tc.operation, func(t *testing.T) {
			f := retainedLedger(t, tc.operation, tc.token)
			r := f.prepare(t)
			// This project owner did not exist when the paid work was accepted.
			account := acctID(t)
			acctExec(t, f.Pool, `INSERT INTO olp.aggregate_budget_accounts(id,project_id) VALUES($1,$2)`, account, f.project)
			var monetary, tokens bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT olp.has_pending_resource_usage($1::uuid),olp.has_pending_retained_tokens($1::uuid)`, account).Scan(&monetary, &tokens); err != nil || !monetary || tokens != tc.token {
				t.Fatalf("late budget liability monetary=%v tokens=%v: %v", monetary, tokens, err)
			}
			if err := f.store.Tombstone(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}
			acctExec(t, f.Pool, `UPDATE olp.provider_resources SET expires_at=now()-interval '1 hour' WHERE id=$1`, r.UUID)
			restarted := resources.New(f.Pool)
			if _, err := restarted.Expire(t.Context(), time.Now().Add(48*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Get(t.Context(), r.Kind, f.Key, r.ID); !errors.Is(err, resources.ErrNotFound) {
				t.Fatalf("deleted resource remained usable: %v", err)
			}
			tombstone, err := restarted.GetByUUID(t.Context(), r.UUID, f.Key)
			if err != nil || tombstone.State != resources.StateDeleted {
				t.Fatalf("retained liability tombstone removed: %+v %v", tombstone, err)
			}
			if err = f.Pool.QueryRow(t.Context(), `SELECT olp.has_pending_resource_usage($1::uuid)`, account).Scan(&monetary); err != nil || !monetary {
				t.Fatalf("deletion erased unresolved budget liability: %v %v", monetary, err)
			}
			var payload string
			if err = f.Pool.QueryRow(t.Context(), `SELECT templates::text FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&payload); err != nil || strings.Contains(payload, "training_examples") || strings.Contains(payload, "conversation_history") {
				t.Fatalf("settlement retained source content: %s %v", payload, err)
			}
		})
	}
}

func TestRetainedSettlementConflictingTotalsKeepVerifiedTermination(t *testing.T) {
	for _, worker := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "worker"}[worker], func(t *testing.T) {
			f := retainedLedger(t, "fine_tuning", true)
			r := f.prepare(t)
			total := retainedTrainingObservation(r, 1_000_000)
			prefix := total
			prefix.Lines = append([]resources.RetainedEvidenceLine(nil), total.Lines...)
			prefix.Lines[0].End = total.Lines[0].Start.Add(total.Lines[0].End.Sub(total.Lines[0].Start) / 2).Truncate(time.Microsecond)
			prefix.Lines[0].Usage.TrainingTokens = new(int64(400_000))
			if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "operator-prefix", strings.Repeat("a", 64), "operator_verified", f.User, prefix, nil); err != nil {
				t.Fatal(err)
			}
			total.RemoteTerminated, total.CoverageComplete = true, true
			var err error
			if worker {
				claims, claimErr := f.store.ClaimRetained(t.Context(), 1)
				if claimErr != nil || len(claims) != 1 {
					t.Fatalf("claim: %+v %v", claims, claimErr)
				}
				err = f.store.CommitRetainedPoll(t.Context(), claims[0], r, total, false)
			} else {
				_, err = f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "provider-total", strings.Repeat("b", 64), "provider_verified", "", total, nil)
			}
			if !errors.Is(err, resources.ErrEvidenceConflict) {
				t.Fatalf("conflicting total accepted: %v", err)
			}
			var terminated bool
			var terminalAt *time.Time
			var state string
			if err = f.Pool.QueryRow(t.Context(), `SELECT remote_terminated,terminal_at,state FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&terminated, &terminalAt, &state); err != nil || !terminated || terminalAt == nil || state != "evidence_missing" {
				t.Fatalf("termination proof lost: %v %v %s %v", terminated, terminalAt, state, err)
			}
			assertRetainedLedgerCounts(t, f, r, 2, 1, 1)
			tail := total
			tail.RemoteTerminated = false
			tail.Lines = append([]resources.RetainedEvidenceLine(nil), total.Lines...)
			tail.Lines[0].Start = prefix.Lines[0].End
			tail.Lines[0].Usage.TrainingTokens = new(int64(600_000))
			if _, err = f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "operator-tail", strings.Repeat("c", 64), "operator_verified", f.User, tail, nil); err != nil {
				t.Fatal(err)
			}
			if err = f.Pool.QueryRow(t.Context(), `SELECT state FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&state); err != nil || state != "closed" {
				t.Fatalf("complete nonoverlapping evidence cannot close: %s %v", state, err)
			}
			assertRetainedLedgerCounts(t, f, r, 3, 2, 2)
		})
	}
}

func TestRetainedSettlementLateFinishTimestampDefinesCoverage(t *testing.T) {
	f := retainedLedger(t, "fine_tuning", true)
	r := f.prepare(t)
	observed := time.Now().UTC().Truncate(time.Microsecond)
	started, finished := observed.Add(-time.Hour), observed.Add(-time.Minute)
	metadata := map[string]any{"upstream_model": "retained-model", "created_at": started.Format(time.RFC3339Nano), "terminal_observed_at": observed.Format(time.RFC3339Nano)}
	r.Metadata, _ = json.Marshal(metadata)
	if err := f.store.SaveRetainedResult(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	termination := resources.RetainedObservation{RemoteTerminated: true, ObservedAt: observed}
	if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "first-terminal", strings.Repeat("a", 64), "provider_verified", "", termination, nil); err != nil {
		t.Fatal(err)
	}
	metadata["finished_at"] = finished.Format(time.RFC3339Nano)
	r.Metadata, _ = json.Marshal(metadata)
	if err := f.store.SaveRetainedResult(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	complete := resources.RetainedObservation{RemoteTerminated: true, CoverageComplete: true, ObservedAt: finished, Lines: []resources.RetainedEvidenceLine{{Component: "training", Model: "retained-model", Start: started, End: finished, Usage: usage.AttemptUsage{TrainingTokens: new(int64(1_000_000))}}}}
	if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "complete-total", strings.Repeat("b", 64), "provider_verified", "", complete, nil); err != nil {
		t.Fatal(err)
	}
	var state string
	var terminalAt time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT state,terminal_at FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&state, &terminalAt); err != nil || state != "closed" || !terminalAt.Equal(observed) {
		t.Fatalf("native coverage did not close independently of termination observation: %s %v %v", state, terminalAt, err)
	}
}

func TestRetainedSettlementFinalEvidencePrecedesTermination(t *testing.T) {
	f := retainedLedger(t, "fine_tuning", true)
	r := f.prepare(t)
	final := retainedTrainingObservation(r, 1_000_000)
	final.CoverageComplete = true
	for range 2 {
		if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "final-operator-usage", strings.Repeat("a", 64), "operator_verified", f.User, final, nil); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.Pool.QueryRow(t.Context(), `SELECT state FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&state); err != nil || state != "evidence_missing" {
		t.Fatalf("unverified termination closed liability: %s %v", state, err)
	}
	termination := resources.RetainedObservation{RemoteTerminated: true, ObservedAt: time.Now().UTC()}
	if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "provider-termination", strings.Repeat("b", 64), "provider_verified", "", termination, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT state FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&state); err != nil || state != "closed" {
		t.Fatalf("independently received final coverage and termination did not close: %s %v", state, err)
	}
	assertRetainedLedgerCounts(t, f, r, 2, 1, 1)
}

func TestRetainedSettlementTerminalCoverageAndCancellationUsage(t *testing.T) {
	f := retainedLedger(t, "fine_tuning", true)
	r := f.prepare(t)
	// Cancellation is remote lifecycle state; incurred training remains payable.
	acctExec(t, f.Pool, `UPDATE olp.provider_resources SET state='cancelled' WHERE id=$1`, r.UUID)
	complete := retainedTrainingObservation(r, 1_000_000)
	terminated := resources.RetainedObservation{ObservedAt: complete.ObservedAt, RemoteTerminated: true}
	if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "verified-cancellation", strings.Repeat("a", 64), "provider_verified", "", terminated, nil); err != nil {
		t.Fatal(err)
	}
	assertState := func(want string) {
		t.Helper()
		var state string
		if err := f.Pool.QueryRow(t.Context(), `SELECT state FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&state); err != nil || state != want {
			t.Fatalf("settlement state=%s want=%s error=%v", state, want, err)
		}
	}
	assertState("evidence_missing")
	// A terminal assertion omitting quantity does not supply a zero bill.
	terminated.CoverageComplete = true
	if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "omitted-usage", strings.Repeat("b", 64), "operator_verified", f.User, terminated, nil); err != nil {
		t.Fatal(err)
	}
	assertState("evidence_missing")
	complete.CoverageComplete = true
	if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "complete-usage", strings.Repeat("c", 64), "operator_verified", f.User, complete, nil); err != nil {
		t.Fatal(err)
	}
	assertState("closed")
	complete.CoverageComplete = false
	if _, err := resources.New(f.Pool).ApplyRetainedEvidence(t.Context(), r.UUID, "cross-source-duplicate", strings.Repeat("d", 64), "provider_verified", "", complete, nil); err != nil {
		t.Fatal(err)
	}
	assertState("closed")
	var training int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT sum(training_tokens) FROM olp.attempt_usage_facts WHERE request_id IN (SELECT event_id FROM olp.retained_usage_coverage WHERE resource_id=$1)`, r.UUID).Scan(&training); err != nil || training != 1_000_000 {
		t.Fatalf("cancelled paid usage was lost or counted twice: %d %v", training, err)
	}
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT olp.has_pending_resource_usage($1::uuid)`, f.Key).Scan(&pending); err != nil || pending {
		t.Fatalf("complete cancelled work retained unresolved liability: %v %v", pending, err)
	}
}

func TestRetainedSettlementWorkerFencesAndRevokedCredentials(t *testing.T) {
	f := retainedLedger(t, "fine_tuning", true)
	r := f.prepare(t)
	claims, err := f.store.ClaimRetained(t.Context(), 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("initial worker claim: %+v %v", claims, err)
	}
	old := claims[0]
	acctExec(t, f.Pool, `UPDATE olp.retained_settlements SET lease_until=now()-interval '1 second' WHERE resource_id=$1`, r.UUID)
	claims, err = resources.New(f.Pool).ClaimRetained(t.Context(), 1)
	if err != nil || len(claims) != 1 || claims[0].ClaimID == old.ClaimID {
		t.Fatalf("replacement worker claim: %+v %v", claims, err)
	}
	if err = f.store.CommitRetainedPoll(t.Context(), old, r, resources.RetainedObservation{}, false); !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("stale worker committed: %v", err)
	}
	if err = f.store.HeartbeatRetained(t.Context(), old); !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("stale worker extended lease: %v", err)
	}
	if err = f.store.FinishRetainedClaim(t.Context(), claims[0], ""); err != nil {
		t.Fatal(err)
	}
	acctExec(t, f.Pool, `UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1`, f.credential)
	acctExec(t, f.Pool, `UPDATE olp.retained_settlements SET poll_after=now() WHERE resource_id=$1`, r.UUID)
	worker := &retainedworker.Worker{Store: resources.New(f.Pool), Resolver: resources.NewResolver(f.Pool)}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err = worker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var state, errorClass string
	if err = f.Pool.QueryRow(t.Context(), `SELECT state,last_error FROM olp.retained_settlements WHERE resource_id=$1`, r.UUID).Scan(&state, &errorClass); err != nil || state != "evidence_missing" || errorClass != "credential_or_pin_unavailable" {
		t.Fatalf("revocation dropped liability: %s %s %v", state, errorClass, err)
	}
	var pending bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT olp.has_pending_resource_usage($1::uuid)`, f.Key).Scan(&pending); err != nil || !pending {
		t.Fatalf("revoked credential erased pending budget owner: %v %v", pending, err)
	}
}

func TestRetainedSettlementPreparedAndRejectedIntentsCannotRedispatch(t *testing.T) {
	for _, preparedOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "crash_before_dispatch", false: "provider_rejection"}[preparedOnly], func(t *testing.T) {
			f := retainedLedger(t, "fine_tuning", true)
			r, fresh, err := f.store.PrepareLifecycle(t.Context(), f.resource, "fine_tuning", "not-accepted", bytes.Repeat([]byte{9}, 32))
			if err != nil || !fresh {
				t.Fatalf("prepare: %v %v", fresh, err)
			}
			if preparedOnly {
				acctExec(t, f.Pool, `UPDATE olp.retained_submissions SET created_at=now()-interval '3 minutes' WHERE resource_id=$1`, r.UUID)
				claims, err := f.store.ClaimRetained(t.Context(), 1)
				if err != nil || len(claims) != 1 {
					t.Fatalf("claim: %+v %v", claims, err)
				}
				closed, err := f.store.CloseUndispatchedRetained(t.Context(), claims[0])
				if err != nil || !closed {
					t.Fatalf("undispatched proof: %v %v", closed, err)
				}
			} else {
				if err = f.store.DispatchLifecycle(t.Context(), r.UUID); err != nil {
					t.Fatal(err)
				}
				if err = f.store.RejectRetained(t.Context(), r, "not-accepted", "fine_tuning:create", true); err != nil {
					t.Fatal(err)
				}
			}
			if err = f.store.DispatchLifecycle(t.Context(), r.UUID); !errors.Is(err, resources.ErrSubmissionConflict) {
				t.Fatalf("non-accepted intent redispatched: %v", err)
			}
			if err = f.store.UnknownLifecycle(t.Context(), r.UUID); err != nil {
				t.Fatal(err)
			}
			var state string
			var pending bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT s.state,olp.has_pending_resource_usage(r.api_key_id) FROM olp.retained_settlements s JOIN olp.provider_resources r ON r.id=s.resource_id WHERE r.id=$1 AND r.state='not_accepted'`, r.UUID).Scan(&state, &pending); err != nil || state != "closed" || pending {
				t.Fatalf("known absence retained liability: %s %v %v", state, pending, err)
			}
			assertRetainedLedgerCounts(t, f, r, 0, 0, 0)
		})
	}
}

func TestRetainedSettlementLateGroupAndSupplyOwnersGetDurableBalances(t *testing.T) {
	f := retainedLedger(t, "fine_tuning", true)
	var routeID string
	if err := f.Pool.QueryRow(t.Context(), `SELECT route_id::text FROM olp.route_revisions WHERE id=$1`, f.resource.RouteRevisionID).Scan(&routeID); err != nil {
		t.Fatal(err)
	}
	owners := []string{f.Provider, f.resource.SlotID, routeID}
	f.resource.RetainedAccounting.Templates["retained-model"].Attempts[0].Routing.Budgets = append([]string(nil), owners...)
	r := f.prepare(t)
	group := acctID(t)
	acctExec(t, f.Pool, `INSERT INTO olp.budget_groups(id,name,project_id,daily_cost_limit,etag,created_by) VALUES($1,'Late retained group',$2,100,$3,$4)`, group, f.project, acctID(t), f.User)
	acctExec(t, f.Pool, `UPDATE olp.api_keys SET budget_group_id=$2 WHERE id=$1`, f.Key, group)
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT olp.has_pending_resource_usage($1::uuid)`, group).Scan(&pending); err != nil || !pending {
		t.Fatalf("late group lost pending owner: %v %v", pending, err)
	}
	observation := retainedTrainingObservation(r, 1_000_000)
	observation.RemoteTerminated = true
	observation.CoverageComplete = true
	if _, err := f.store.ApplyRetainedEvidence(t.Context(), r.UUID, "late-owner-usage", strings.Repeat("e", 64), "provider_verified", "", observation, nil); err != nil {
		t.Fatal(err)
	}
	var supplyOwners int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(DISTINCT owner_id) FROM olp.supply_cost_windows WHERE owner_id=ANY($1::uuid[]) AND window_kind='day' AND accrued=1`, owners).Scan(&supplyOwners); err != nil || supplyOwners != 3 {
		t.Fatalf("late supply balances=%d %v", supplyOwners, err)
	}
	var groupCost string
	if err := f.Pool.QueryRow(t.Context(), `SELECT accrued::text FROM olp.budget_group_cost_windows WHERE budget_group_id=$1 AND window_kind='day'`, group).Scan(&groupCost); err != nil || strings.TrimRight(strings.TrimRight(groupCost, "0"), ".") != "1" {
		t.Fatalf("late group balance=%s %v", groupCost, err)
	}
	owners = append(owners, f.Key, group)
	var versions int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debit_versions WHERE owner_id=ANY($1::uuid[]) AND version>0`, owners).Scan(&versions); err != nil || versions != 5 {
		t.Fatalf("settlement did not fence stale limiter balances: %d %v", versions, err)
	}
}

//go:build integration

package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/runtime"
)

// claimTestEnv names a PostgreSQL admin connection the test creates and drops
// a scratch database on. The integration suite provisions it.
const claimTestEnv = "OLP_TEST_DATABASE_ADMIN_URL"

func claimPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv(claimTestEnv)
	if admin == "" {
		t.Fatalf("%s is required; run make integration", claimTestEnv)
	}
	cfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("olp_media_claim_%s", strings.ReplaceAll(uuid.NewString()[:13], "-", ""))
	adminPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adminPool.Exec(t.Context(), "CREATE DATABASE "+dbName); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = dbName
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database: %v", err)
		}
		adminPool.Close()
	})
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// claimUpstream is a scripted OpenAI-family media provider for worker tests.
type claimUpstream struct {
	srv         *httptest.Server
	getStatus   atomic.Value // string
	getCalls    atomic.Int64
	deleteCalls atomic.Int64
	delete404   atomic.Bool
}

func newClaimUpstream(t *testing.T) *claimUpstream {
	u := &claimUpstream{}
	u.getStatus.Store("completed")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/videos/{id}", func(w http.ResponseWriter, r *http.Request) {
		u.getCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"video","status":%q,"model":"upstream-video-model","progress":100,"created_at":1800000000,"completed_at":1800000060,"seconds":"8","size":"1280x720"}`,
			r.PathValue("id"), u.getStatus.Load().(string))
	})
	mux.HandleFunc("DELETE /v1/videos/{id}", func(w http.ResponseWriter, r *http.Request) {
		if u.delete404.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":{"message":"video not found","type":"invalid_request_error","code":"not_found"}}`)
			return
		}
		u.deleteCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"video.deleted","deleted":true}`, r.PathValue("id"))
	})
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

// claimFixture is a seeded installation slice plus a media Service whose
// transport reaches the scripted upstream. auth_mode none keeps credentials
// out of the worker path entirely.
type claimFixture struct {
	pool         *pgxpool.Pool
	service      *Service
	upstream     *claimUpstream
	gaps         *atomic.Uint64
	generationID string
	providerID   string
	revisionID   string
	slotID       string
	apiKeyID     string
}

const claimVideoModel = "upstream-video-model"

func claimVideoCapabilities() []runtime.Capability {
	out := []runtime.Capability{
		{Model: claimVideoModel, Operation: OpVideoCreate, Surface: "openai", Mode: "async"},
		{Model: claimVideoModel, Operation: OpVideoList, Surface: "openai", Mode: "unary"},
	}
	for _, op := range []string{OpVideoGet, OpVideoContent, OpVideoDelete} {
		out = append(out, runtime.Capability{Model: claimVideoModel, Operation: op, Surface: "openai", Mode: "unary"})
	}
	return out
}

func seedClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	f := &claimFixture{pool: claimPool(t), upstream: newClaimUpstream(t)}
	ctx := t.Context()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	ownerID := uuid.NewString()
	exec("INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,$2,'Owner','owner',$3)",
		ownerID, "owner-"+uuid.NewString()[:8]+"@example.test", uuid.NewString())
	f.apiKeyID = uuid.NewString()
	exec(`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag)
		VALUES($1,$2,$3,'media claim test',$4,'{"scopes":["inference"],"allowed_routes":[]}'::jsonb,$5)`,
		f.apiKeyID, "claimLookup"+uuid.NewString()[:8], []byte{1, 2, 3}, ownerID, uuid.NewString())

	f.providerID = uuid.NewString()
	f.revisionID = uuid.NewString()
	f.slotID = uuid.NewString()
	endpoint := f.upstream.srv.URL + "/v1"
	configuration := map[string]any{
		"kind": "openai", "auth_mode": "none", "endpoint": endpoint,
		"cloud_region": "", "cloud_project": "", "deployment": "", "api_version": "",
		"options": map[string]any{"models": map[string]any{}, "credential_headers": []string{},
			"parameter_defaults": map[string]any{}, "vendor_id": ""},
	}
	capabilities := []map[string]any{}
	for _, c := range claimVideoCapabilities() {
		capabilities = append(capabilities, map[string]any{
			"operation": c.Operation, "surface": c.Surface, "mode": c.Mode, "source": "certified"})
	}
	models := []map[string]any{{
		"id": uuid.NewString(), "upstream_model": claimVideoModel,
		"display_name": "Video model", "capabilities": capabilities,
	}}
	slot := map[string]any{
		"id": f.slotID, "name": "Default", "enabled": true, "priority": 0, "weight": 1,
		"credential_id": nil, "credential_version": nil, "default": true,
	}
	configJSON, _ := json.Marshal(configuration)
	modelsJSON, _ := json.Marshal(models)
	slotsJSON, _ := json.Marshal([]map[string]any{slot})
	exec(`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,active_revision_id)
		VALUES($1,'media-claim-provider','openai','active',$2,$3,$4,$5,$6)`,
		f.providerID, configJSON, uuid.NewString(), uuid.NewString(), ownerID, f.revisionID)
	exec(`INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by)
		VALUES($1,$2,1,'media-claim-provider',$3,$4,$5,$6,$7,$8)`,
		f.revisionID, f.providerID, configJSON, modelsJSON, slotsJSON, nil, uuid.NewString(), ownerID)

	f.generationID = uuid.NewString()
	routeID := uuid.NewString()
	provider := runtime.Provider{
		ID: f.providerID, Name: "media-claim-provider", Kind: "openai", Enabled: true,
		RevisionID: f.revisionID, Endpoint: endpoint, AuthMode: "none",
		Capabilities: claimVideoCapabilities(),
		Slots:        []runtime.Slot{{ID: f.slotID, Name: "Default", Enabled: true, Weight: 1}},
	}
	snapshot := &runtime.Snapshot{
		Generation: runtime.Generation{ID: f.generationID, Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]runtime.Provider{f.providerID: provider},
		Routes: map[string]runtime.Route{"video-default": {
			ID: routeID, Slug: "video-default", RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(),
			Operations:     []string{OpVideoCreate, OpVideoList, OpVideoGet, OpVideoContent, OpVideoDelete},
			OverallTimeout: 8000, MaxAttempts: 1, RoutingID: routeID,
			Targets: []runtime.Target{{
				ID: uuid.NewString(), ProviderID: f.providerID, ProviderModel: claimVideoModel,
				Priority: 0, Weight: 1, Timeout: 6000, RoutingID: uuid.NewString(),
			}},
		}},
	}
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO olp.runtime_releases(id,sequence,sha256,snapshot,created_by) VALUES($1,1,$2,$3,$4)",
		f.generationID, digest, encoded, ownerID)

	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	f.gaps = &atomic.Uint64{}
	f.service = &Service{
		Pool: f.pool,
		Transport: &Transport{
			Client:           policy.Client(5 * time.Minute),
			Auth:             connectors.NewAuth(policy),
			Egress:           policy,
			Spool:            testSpool(t, MinCapacityBytes),
			MaxResponseBytes: 16 << 20,
		},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Gaps: f.gaps,
	}
	return f
}

// claimJobSeed describes one durable job row. Triggers only fire on UPDATE,
// so an INSERT can set updated_at directly — that is how a stale 'creating'
// reservation becomes claimable without waiting.
type claimJobSeed struct {
	UpstreamID  *string
	State       State
	Lifecycle   Lifecycle
	Progress    *float32
	Content     bool
	ExpiresAt   *time.Time
	LastPolled  *time.Time
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
	NextReconAt *time.Time
	DeletedAt   *time.Time
}

func (f *claimFixture) insertJob(t *testing.T, seed claimJobSeed) JobRecord {
	t.Helper()
	if seed.State == "" {
		seed.State = StateQueued
	}
	if seed.Lifecycle == "" {
		seed.Lifecycle = LifecycleActive
	}
	var completedAt *time.Time
	if seed.State == StateSucceeded || seed.State == StateFailed || seed.State == StateCancelled {
		at := time.Now()
		completedAt = &at
	}
	id := uuid.NewString()
	_, err := f.pool.Exec(t.Context(), `INSERT INTO olp.media_jobs (
			id, upstream_job_id, api_key_id, provider_id, provider_model, route_slug,
			operation, surface, state, lifecycle_state, progress_percent, content_available,
			expires_at, error_class, completed_at, last_polled_at, deleted_at,
			runtime_generation_id, provider_revision_id, slot_id, etag,
			next_reconciliation_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,'video-default','video_create','openai',$6,$7,$8::real::numeric,$9,
			$10,NULL,$11,$12,$13,$14,$15,$16,$17,
			COALESCE($18, now()), COALESCE($19, now()), COALESCE($20, now()))`,
		id, seed.UpstreamID, f.apiKeyID, f.providerID, claimVideoModel,
		string(seed.State), string(seed.Lifecycle), seed.Progress, seed.Content,
		seed.ExpiresAt, completedAt, seed.LastPolled, seed.DeletedAt,
		f.generationID, f.revisionID, f.slotID, uuid.NewString(),
		seed.NextReconAt, seed.CreatedAt, seed.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	record, err := Job(t.Context(), f.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// forceClaimable expires any outstanding lease and makes the job due again —
// the deterministic form of a lease handoff. The row guard stamps updated_at
// on every update, so aging runs inside one transaction with triggers
// suspended, matching how a genuinely stale 'creating' row looks.
func (f *claimFixture) forceClaimable(t *testing.T, id string) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "ALTER TABLE olp.media_jobs DISABLE TRIGGER ALL"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.media_jobs SET
			next_reconciliation_at = now() - interval '1 second',
			reconciliation_claimed_until = CASE WHEN reconciliation_claim_id IS NULL
				THEN NULL ELSE now() - interval '1 second' END,
			updated_at = now() - interval '10 minutes'
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE olp.media_jobs ENABLE TRIGGER ALL"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// claim makes the job claimable, runs one bounded claim batch, and returns
// this worker's claimed record.
func (f *claimFixture) claim(t *testing.T, id string) JobRecord {
	t.Helper()
	f.forceClaimable(t, id)
	records, err := ClaimJobs(t.Context(), f.pool, time.Now(), 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.ID == id {
			return record
		}
	}
	t.Fatalf("job %s was not claimed", id)
	return JobRecord{}
}

// leaseOf reads the raw claim pair off the row.
func (f *claimFixture) leaseOf(t *testing.T, id string) (*string, *time.Time) {
	t.Helper()
	var claimID *string
	var until *time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT reconciliation_claim_id::text,
		reconciliation_claimed_until FROM olp.media_jobs WHERE id = $1`, id).
		Scan(&claimID, &until); err != nil {
		t.Fatal(err)
	}
	return claimID, until
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// requireRowUnchanged asserts the fields any stale write could touch stayed
// identical — every mutation path rotates the etag, and claim or lease drift
// would show in the claim pair.
func requireRowUnchanged(t *testing.T, f *claimFixture, id string, before JobRecord, beforeUntil *time.Time) {
	t.Helper()
	after, err := Job(t.Context(), f.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	claimID, until := f.leaseOf(t, id)
	switch {
	case after.ETag != before.ETag:
		t.Fatalf("stale worker mutated the row: etag %s -> %s", before.ETag, after.ETag)
	case !equalTimePtr(beforeUntil, until):
		t.Fatalf("stale worker moved the lease: %v -> %v", beforeUntil, until)
	case !equalTimePtr(before.LastPolledAt, after.LastPolledAt):
		t.Fatalf("stale worker moved last_polled_at: %v -> %v", before.LastPolledAt, after.LastPolledAt)
	case before.Lifecycle != after.Lifecycle || before.State != after.State:
		t.Fatalf("stale worker changed state: %s/%s -> %s/%s",
			before.Lifecycle, before.State, after.Lifecycle, after.State)
	case before.ReconciliationAttempts != after.ReconciliationAttempts:
		t.Fatalf("attempt counter moved: %d -> %d", before.ReconciliationAttempts, after.ReconciliationAttempts)
	case !before.NextReconciliationAt.Equal(after.NextReconciliationAt):
		t.Fatalf("schedule moved: %v -> %v", before.NextReconciliationAt, after.NextReconciliationAt)
	case (before.ReconciliationError == nil) != (after.ReconciliationError == nil) ||
		(before.ReconciliationError != nil && *before.ReconciliationError != *after.ReconciliationError):
		t.Fatalf("reconciliation error moved: %v -> %v", before.ReconciliationError, after.ReconciliationError)
	case (claimID == nil) != (before.ReconciliationClaimID == nil) ||
		(claimID != nil && *claimID != *before.ReconciliationClaimID):
		t.Fatalf("claim moved unexpectedly: %v -> %v", before.ReconciliationClaimID, claimID)
	}
}

func strPtr(value string) *string { return &value }

// Worker A claims a job; ownership moves to B; A's lifecycle transition is
// refused without changing B's row or lease.
func TestStaleClaimCannotTransitionLifecycle(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()
	old := time.Now().Add(-10 * time.Minute)
	job := f.insertJob(t, claimJobSeed{Lifecycle: LifecycleCreating, State: StateQueued, UpdatedAt: &old})

	workerA := f.claim(t, job.ID)
	claimA := *workerA.ReconciliationClaimID
	// The lease expires unprocessed and worker B reclaims the same job.
	workerB := f.claim(t, job.ID)
	claimB := *workerB.ReconciliationClaimID
	if claimA == claimB {
		t.Fatal("the second claim must carry a new token")
	}
	_, leaseB := f.leaseOf(t, job.ID)

	// Direct transition under the stale token is refused.
	if _, err := markCreateAmbiguousClaimed(ctx, f.pool, job.ID, claimA,
		"upstream_create_outcome_unknown_after_restart"); !errors.Is(err, errClaimLost) {
		t.Fatalf("stale lifecycle transition: %v", err)
	}
	// And through the operation path the refusal classifies as a handoff.
	record := workerA
	if outcome := f.service.reconcileClaimed(ctx, record); outcome != outcomeHandedOff {
		t.Fatalf("stale operation outcome %d", outcome)
	}
	requireRowUnchanged(t, f, job.ID, workerB, leaseB)
	if got := f.gaps.Load(); got != 0 {
		t.Fatalf("claim loss must not count as a persistence gap: %d", got)
	}
}

// Worker A starts a poll; B reclaims and persists newer state; A's delayed
// successful response cannot overwrite it.
func TestStaleClaimCannotApplyPollResult(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()
	polled := time.Now().Add(-time.Minute)
	job := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-claim-poll"), State: StateQueued,
		Lifecycle: LifecycleActive, LastPolled: &polled})

	workerA := f.claim(t, job.ID)
	claimA := *workerA.ReconciliationClaimID
	// A holds the claim across dispatch exactly as executeReconciliation does.
	owned, err := ExtendClaim(ctx, f.pool, job.ID, claimA, time.Now().Add(time.Minute))
	if err != nil || !owned {
		t.Fatalf("extend own claim: owned=%v err=%v", owned, err)
	}

	// Ownership moves to B while A's response is in flight.
	workerB := f.claim(t, job.ID)
	claimB := *workerB.ReconciliationClaimID
	// The stale token no longer revalidates.
	if owned, err := ExtendClaim(ctx, f.pool, job.ID, claimA, time.Now().Add(time.Minute)); err != nil || owned {
		t.Fatalf("stale extend: owned=%v err=%v", owned, err)
	}
	// B persists a newer poll result under its own claim.
	progress50 := float32(50)
	polledB := time.Now()
	updated, err := refreshJobClaimed(ctx, f.pool, job.ID, claimB, JobUpdate{
		State: StateRunning, ProgressPercent: &progress50, LastPolledAt: polledB})
	if err != nil {
		t.Fatalf("owner refresh: %v", err)
	}
	if updated.State != StateRunning || updated.ProgressPercent == nil || *updated.ProgressPercent != 50 {
		t.Fatalf("owner refresh applied %+v", updated)
	}
	_, leaseB := f.leaseOf(t, job.ID)

	// A's delayed response is newer on poll time and would land without the
	// fence; the claim check inside the write refuses it.
	progress100 := float32(100)
	expires := polledB.Add(time.Hour)
	if _, err := refreshJobClaimed(ctx, f.pool, job.ID, claimA, JobUpdate{
		State: StateSucceeded, ProgressPercent: &progress100, ContentAvailable: true,
		ExpiresAt: &expires, LastPolledAt: polledB.Add(time.Second)}); !errors.Is(err, errClaimLost) {
		t.Fatalf("stale poll result: %v", err)
	}
	requireRowUnchanged(t, f, job.ID, updated, leaseB)
	row, err := Job(ctx, f.pool, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateRunning || row.ContentAvailable {
		t.Fatalf("stale poll overwrote state: %+v", row)
	}
}

// A delete confirmation arriving after ownership moved to B can neither
// finalize the job nor release B's claim.
func TestStaleClaimCannotFinalizeOrReleaseDelete(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()
	job := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-claim-delete"), State: StateQueued,
		Lifecycle: LifecycleDeletePending})

	workerA := f.claim(t, job.ID)
	claimA := *workerA.ReconciliationClaimID
	workerB := f.claim(t, job.ID)
	claimB := *workerB.ReconciliationClaimID
	_, leaseB := f.leaseOf(t, job.ID)

	// The upstream delete A confirms is real, but the tombstone write is
	// fenced on ownership — it refuses.
	if err := finalizeDeletionClaimed(ctx, f.pool, job.ID, claimA); !errors.Is(err, errClaimLost) {
		t.Fatalf("stale finalize: %v", err)
	}
	// The stale checkpoint cannot clear the current owner's claim.
	err := FinishReconciliation(ctx, f.pool, job.ID, claimA, time.Now().Add(24*time.Hour), nil)
	var jobErr *JobError
	if !errors.As(err, &jobErr) || jobErr.Kind != JobErrorPrecondition {
		t.Fatalf("stale finish: %v", err)
	}
	requireRowUnchanged(t, f, job.ID, workerB, leaseB)
	row, err := Job(ctx, f.pool, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Lifecycle != LifecycleDeletePending || row.DeletedAt != nil {
		t.Fatalf("stale worker finalized: %+v", row)
	}

	// The current owner completes the same sequence cleanly.
	if err := finalizeDeletionClaimed(ctx, f.pool, job.ID, claimB); err != nil {
		t.Fatalf("owner finalize: %v", err)
	}
	if err := FinishReconciliation(ctx, f.pool, job.ID, claimB, time.Now().Add(24*time.Hour), nil); err != nil {
		t.Fatalf("owner finish: %v", err)
	}
	row, err = Job(ctx, f.pool, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimID, until := f.leaseOf(t, job.ID)
	if row.Lifecycle != LifecycleDeleted || row.DeletedAt == nil || row.ContentAvailable ||
		claimID != nil || until != nil {
		t.Fatalf("owner tombstone: %+v claim=%v", row, claimID)
	}
}

// Claim loss at completion is a benign handoff; genuine persistence failures
// keep their existing reporting.
func TestCompletionClassifiesClaimLossAsHandoff(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()

	// The operation already finished the tombstone; only the checkpoint
	// remains, and the lease now belongs to B who released it cleanly.
	handedOff := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-finish-handoff"), State: StateQueued,
		Lifecycle: LifecycleDeletePending})
	recordA := f.claim(t, handedOff.ID)
	recordB := f.claim(t, handedOff.ID)
	if err := FinishReconciliation(ctx, f.pool, handedOff.ID, *recordB.ReconciliationClaimID,
		time.Now().Add(24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	recordA.Lifecycle = LifecycleDeleted
	before := f.gaps.Load()
	if outcome := f.service.reconcileClaimed(ctx, recordA); outcome != outcomeHandedOff {
		t.Fatalf("claim loss at completion: outcome %d", outcome)
	}
	if got := f.gaps.Load(); got != before {
		t.Fatalf("handoff recorded a gap: %d -> %d", before, got)
	}

	// A genuinely missing record keeps failure reporting.
	gone := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-finish-missing"), State: StateQueued,
		Lifecycle: LifecycleDeletePending})
	recordG := f.claim(t, gone.ID)
	if _, err := f.pool.Exec(ctx, "DELETE FROM olp.media_jobs WHERE id = $1", gone.ID); err != nil {
		t.Fatal(err)
	}
	recordG.Lifecycle = LifecycleDeleted
	before = f.gaps.Load()
	if outcome := f.service.reconcileClaimed(ctx, recordG); outcome != outcomeFailed {
		t.Fatalf("missing record at completion: outcome %d", outcome)
	}
	if got := f.gaps.Load(); got != before+1 {
		t.Fatalf("missing record gap: %d -> %d", before, got)
	}

	// A database error keeps failure reporting too.
	stuck := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-finish-error"), State: StateQueued,
		Lifecycle: LifecycleDeletePending})
	recordE := f.claim(t, stuck.ID)
	recordE.Lifecycle = LifecycleDeleted
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before = f.gaps.Load()
	if outcome := f.service.reconcileClaimed(cancelled, recordE); outcome != outcomeFailed {
		t.Fatalf("database error at completion: outcome %d", outcome)
	}
	if got := f.gaps.Load(); got != before+1 {
		t.Fatalf("database error gap: %d -> %d", before, got)
	}
}

// A poll arriving after the delete completed must not rotate the tombstone's
// ETag or rewrite its metadata; content stays unavailable.
func TestLatePollLeavesTombstoneUntouched(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()
	progress := float32(100)
	polled := time.Now().Add(-time.Hour)
	expires := time.Now().Add(time.Hour)
	job := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-late-poll"), State: StateSucceeded,
		Lifecycle: LifecycleDeletePending, Progress: &progress, Content: true,
		LastPolled: &polled, ExpiresAt: &expires})

	owner := f.claim(t, job.ID)
	claimA := *owner.ReconciliationClaimID
	if err := finalizeDeletionClaimed(ctx, f.pool, job.ID, claimA); err != nil {
		t.Fatal(err)
	}
	if err := FinishReconciliation(ctx, f.pool, job.ID, claimA, time.Now().Add(24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	tombstone, err := Job(ctx, f.pool, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.Lifecycle != LifecycleDeleted || tombstone.ContentAvailable || tombstone.DeletedAt == nil {
		t.Fatalf("tombstone %+v", tombstone)
	}

	// The stale worker's poll response lands after finalization.
	if _, err := refreshJobClaimed(ctx, f.pool, job.ID, claimA, JobUpdate{
		State: StateSucceeded, ContentAvailable: true, ProgressPercent: &progress,
		ExpiresAt: &expires, LastPolledAt: time.Now()}); !errors.Is(err, errClaimLost) {
		t.Fatalf("late claimed poll: %v", err)
	}
	// A client-path poll is likewise ignored by the lifecycle check.
	later := expires.Add(time.Hour)
	got, err := RefreshJob(ctx, f.pool, job.ID, JobUpdate{
		State: StateSucceeded, ContentAvailable: true, ProgressPercent: &progress,
		ExpiresAt: &later, LastPolledAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got.ETag != tombstone.ETag || got.Lifecycle != LifecycleDeleted {
		t.Fatalf("client poll rewrote tombstone: %+v", got)
	}
	requireRowUnchanged(t, f, job.ID, tombstone, nil)
	row, err := Job(ctx, f.pool, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ContentAvailable || !row.DeletedAt.Equal(*tombstone.DeletedAt) ||
		!equalTimePtr(row.ExpiresAt, tombstone.ExpiresAt) ||
		!equalTimePtr(row.LastPolledAt, tombstone.LastPolledAt) {
		t.Fatalf("tombstone metadata moved: %+v", row)
	}
}

// A current owner still drives the full lifecycle: poll refresh, expiry
// delete, ambiguous-create marking, post-create cleanup, pending-delete
// confirmation, and reclaim after an unfinished lease.
func TestClaimedOwnerCompletesReconciliation(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()
	stalePoll := time.Now().Add(-time.Minute)
	old := time.Now().Add(-10 * time.Minute)
	expired := time.Now().Add(-time.Minute)

	// Worker A claims a delete-pending job then loses the lease without
	// finishing; the pass below must adopt it.
	recovery := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-recovery"), State: StateQueued,
		Lifecycle: LifecycleDeletePending})
	recovered := f.claim(t, recovery.ID)
	if recovered.ReconciliationClaimID == nil {
		t.Fatal("recovery job was not claimed")
	}
	f.forceClaimable(t, recovery.ID)

	polled := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-poll"), State: StateQueued,
		Lifecycle: LifecycleActive, LastPolled: &stalePoll})
	expiring := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-expired"), State: StateSucceeded,
		Lifecycle: LifecycleActive, ExpiresAt: &expired, Content: true})
	ambiguous := f.insertJob(t, claimJobSeed{
		State: StateQueued, Lifecycle: LifecycleCreating, UpdatedAt: &old})
	postCreate := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-stale-create"), State: StateQueued,
		Lifecycle: LifecycleCreating, UpdatedAt: &old})
	cleanup := f.insertJob(t, claimJobSeed{
		UpstreamID: strPtr("upstream-video-cleanup"), State: StateQueued,
		Lifecycle: LifecycleCreateCleanupPending})

	pass, err := f.service.ReconcileOnce(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Claimed != 6 || pass.Completed != 5 || pass.Failed != 1 || pass.HandedOff != 0 {
		t.Fatalf("reconciliation pass %+v", pass)
	}
	if got := f.gaps.Load(); got != 0 {
		t.Fatalf("clean run recorded a gap: %d", got)
	}

	row, err := Job(ctx, f.pool, polled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateSucceeded || row.ProgressPercent == nil || *row.ProgressPercent != 100 ||
		!row.ContentAvailable || row.Lifecycle != LifecycleActive || row.ReconciliationClaimID != nil {
		t.Fatalf("polled job %+v", row)
	}
	for _, id := range []string{expiring.ID, postCreate.ID, cleanup.ID, recovery.ID} {
		row, err := Job(ctx, f.pool, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Lifecycle != LifecycleDeleted || row.DeletedAt == nil || row.ContentAvailable {
			t.Fatalf("job %s not tombstoned: %+v", id, row)
		}
	}
	row, err = Job(ctx, f.pool, ambiguous.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Lifecycle != LifecycleCreateAmbiguous || row.ReconciliationClaimID != nil ||
		row.ReconciliationError == nil || *row.ReconciliationError != "upstream_create_outcome_unknown" {
		t.Fatalf("ambiguous job %+v", row)
	}
	if got := f.upstream.getCalls.Load(); got != 1 {
		t.Fatalf("poll calls %d", got)
	}
	if got := f.upstream.deleteCalls.Load(); got != 4 {
		t.Fatalf("delete calls %d", got)
	}

	// Terminal state and progress monotonicity survive further writes.
	failed := StateFailed
	errorClass := "provider_error"
	got, err := RefreshJob(ctx, f.pool, polled.ID, JobUpdate{
		State: failed, ProgressPercent: new(float32), ErrorClass: &errorClass,
		LastPolledAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSucceeded || got.ProgressPercent == nil || *got.ProgressPercent != 100 {
		t.Fatalf("terminal job regressed: %+v", got)
	}
}

//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/tests/harness"
)

const videoModel = "sora-2"

type videoFixture struct {
	pool             *pgxpool.Pool
	ownerID          string
	projectID        string
	otherProjectID   string
	keyID            string
	otherKeyID       string
	providerID       string
	revisionID       string
	callerRevisionID string
	credentialID     string
	generationID     string
	slotID           string
}

type seededJob struct {
	key       string
	route     string
	revision  string
	state     media.State
	lifecycle media.Lifecycle
	upstream  string
	createdAt time.Time
}

func newVideoFixture(t *testing.T) videoFixture {
	t.Helper()
	pool, _ := harness.Database(t)
	f := videoFixture{
		pool: pool, ownerID: uuid.NewString(), projectID: uuid.NewString(), otherProjectID: uuid.NewString(),
		keyID: uuid.NewString(), otherKeyID: uuid.NewString(), providerID: uuid.NewString(),
		revisionID: uuid.NewString(), callerRevisionID: uuid.NewString(), credentialID: uuid.NewString(),
		generationID: uuid.NewString(), slotID: uuid.NewString(),
	}
	certified := func(operations ...string) string {
		capabilities := make([]map[string]string, len(operations))
		for i, operation := range operations {
			capabilities[i] = map[string]string{"operation": operation, "surface": "openai", "mode": "unary", "source": "certified"}
		}
		models, err := json.Marshal([]map[string]any{{"upstream_model": videoModel, "capabilities": capabilities}})
		if err != nil {
			t.Fatal(err)
		}
		return string(models)
	}
	snapshot, err := json.Marshal(map[string]any{
		"providers": map[string]any{f.providerID: map[string]any{
			"id": f.providerID, "revision_id": f.revisionID, "name": "video", "kind": "openai",
			"active_credential": f.credentialID,
			"slots":             []map[string]any{{"id": f.slotID, "credential_id": f.credentialID}},
		}},
		"routes": map[string]any{"studio": map[string]any{
			"slug": "studio", "revision_id": uuid.NewString(),
			"targets": []map[string]any{{"provider_id": f.providerID, "provider_model": videoModel}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users (id,email,display_name,role,etag) VALUES ($1::uuid,$1::text || '@example.com','Video','owner',$1::uuid)`, []any{f.ownerID}},
		{`INSERT INTO olp.projects (id,name,etag,created_by) VALUES ($1::uuid,$1::text,$1::uuid,$2), ($3::uuid,$3::text,$3::uuid,$2)`, []any{f.projectID, f.ownerID, f.otherProjectID}},
		{`INSERT INTO olp.api_keys (id,lookup_id,digest,name,created_by,policy,etag,project_id)
			VALUES ($1::uuid,$1::text,decode(repeat('00',32),'hex'),'Video',$3,'{}',$1::uuid,$4),
			       ($2::uuid,$2::text,decode(repeat('00',32),'hex'),'Other',$3,'{}',$2::uuid,$5)`, []any{f.keyID, f.otherKeyID, f.ownerID, f.projectID, f.otherProjectID}},
		{`INSERT INTO olp.providers (id,name,kind,state,configuration,etag,slots_etag,created_by,project_id,active_revision,active_revision_id)
			VALUES ($1::uuid,'video','openai','active','{}',$1::uuid,$1::uuid,$2,$3,1,$4)`, []any{f.providerID, f.ownerID, f.projectID, f.revisionID}},
		{`INSERT INTO olp.provider_credentials (id,provider_id,version) VALUES ($1,$2,1)`, []any{f.credentialID, f.providerID}},
		{`INSERT INTO olp.provider_revisions (id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by)
			VALUES ($1,$3,1,'video','{}',$5,'[]',1,$1,$4),
			       ($2,$3,2,'video','{"credential_source":"caller"}',$5,'[]',NULL,$2,$4)`,
			[]any{f.revisionID, f.callerRevisionID, f.providerID, f.ownerID, certified("video_get", "video_content", "video_delete")}},
		{`INSERT INTO olp.runtime_releases (id,sequence,sha256,snapshot,created_by)
			SELECT $1,coalesce(max(sequence),0)+1,repeat('0',64),$2,$3 FROM olp.runtime_releases`, []any{f.generationID, snapshot, f.ownerID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f videoFixture) reservation() media.Reservation {
	return media.Reservation{
		ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID,
		APIKeyID: f.keyID, ProviderID: f.providerID, UpstreamModel: videoModel, RouteSlug: "studio",
		Operation: "video_create", Surface: "openai", CredentialVersionID: &f.credentialID, SlotID: f.slotID,
	}
}

func (f videoFixture) seed(t *testing.T, job seededJob) string {
	t.Helper()
	id := uuid.NewString()
	key, route, revision, created := job.key, job.route, job.revision, job.createdAt
	if key == "" {
		key = f.keyID
	}
	if route == "" {
		route = "studio"
	}
	if revision == "" {
		revision = f.revisionID
	}
	if created.IsZero() {
		created = time.Now()
	}
	var upstream *string
	if job.upstream != "" {
		upstream = &job.upstream
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO olp.media_jobs
		(id,upstream_job_id,api_key_id,provider_id,provider_model,route_slug,operation,surface,state,lifecycle_state,
		 completed_at,deleted_at,etag,runtime_generation_id,provider_revision_id,credential_version_id,slot_id,
		 strict_contract,created_at,next_reconciliation_at)
		VALUES ($1,$2,$3,$4,$5,$6,'video_create','openai',$7,$8,
		 CASE WHEN $7 IN ('succeeded','failed','cancelled') THEN now() END,
		 CASE WHEN $8 = 'deleted' THEN now() END,
		 gen_random_uuid(),$9,$10,$11,$12,false,$13,$13)`,
		id, upstream, key, f.providerID, videoModel, route, string(job.state), string(job.lifecycle),
		f.generationID, revision, f.credentialID, f.slotID, created); err != nil {
		t.Fatal(err)
	}
	return id
}

func jobErrorKind(t *testing.T, err error) media.JobErrorKind {
	t.Helper()
	var job *media.JobError
	if !errors.As(err, &job) {
		t.Fatalf("error %v is not a media job error", err)
	}
	return job.Kind
}

func float32Pointer(value float32) *float32 { return &value }

func TestVideoJobLifecycleRunsFromReservationToTombstone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)

	stray := f.reservation()
	stray.UpstreamModel = "not-certified"
	if _, err := media.ReserveJob(ctx, f.pool, stray); jobErrorKind(t, err) != media.JobErrorInvalid {
		t.Fatalf("a model outside the pinned release reserved a job: %v", err)
	}
	reserved, err := media.ReserveJob(ctx, f.pool, f.reservation())
	if err != nil {
		t.Fatal(err)
	}
	if reserved.Lifecycle != media.LifecycleCreating || reserved.State != media.StateQueued || reserved.UpstreamJobID != nil || reserved.ProviderName != "video" {
		t.Fatalf("reserved job = %+v", reserved)
	}

	started := time.Now().UTC()
	if _, err = media.AttachUpstream(ctx, f.pool, reserved.ID, " vid_1", media.JobUpdate{State: media.StateQueued, LastPolledAt: started}); jobErrorKind(t, err) != media.JobErrorInvalid {
		t.Fatalf("a padded upstream identity attached: %v", err)
	}
	if _, err = media.AttachUpstream(ctx, f.pool, reserved.ID, "vid_1", media.JobUpdate{State: media.StateQueued, ContentAvailable: true, LastPolledAt: started}); jobErrorKind(t, err) != media.JobErrorInvalid {
		t.Fatalf("a queued job attached with content: %v", err)
	}
	attached, err := media.AttachUpstream(ctx, f.pool, reserved.ID, "vid_1", media.JobUpdate{State: media.StateQueued, LastPolledAt: started})
	if err != nil {
		t.Fatal(err)
	}
	if attached.Lifecycle != media.LifecycleActive || attached.UpstreamJobID == nil || *attached.UpstreamJobID != "vid_1" || attached.ETag == reserved.ETag {
		t.Fatalf("attached job = %+v", attached)
	}
	if retried, err := media.AttachUpstream(ctx, f.pool, reserved.ID, "vid_1", media.JobUpdate{State: media.StateQueued, LastPolledAt: started}); err != nil || retried.ID != reserved.ID {
		t.Fatalf("an ambiguous attach retry with the same identity: %+v, %v", retried, err)
	}
	if _, err = media.AttachUpstream(ctx, f.pool, reserved.ID, "vid_2", media.JobUpdate{State: media.StateQueued, LastPolledAt: started}); jobErrorKind(t, err) != media.JobErrorPrecondition {
		t.Fatalf("an attached job took a second identity: %v", err)
	}
	twin, err := media.ReserveJob(ctx, f.pool, f.reservation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = media.AttachUpstream(ctx, f.pool, twin.ID, "vid_1", media.JobUpdate{State: media.StateQueued, LastPolledAt: started}); jobErrorKind(t, err) != media.JobErrorUpstreamIdentityConflict {
		t.Fatalf("two jobs share one upstream identity: %v", err)
	}

	refresh := func(at time.Duration, update media.JobUpdate) media.JobRecord {
		t.Helper()
		update.LastPolledAt = started.Add(at)
		record, err := media.RefreshJob(ctx, f.pool, reserved.ID, update)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	if _, err = media.RefreshJob(ctx, f.pool, reserved.ID, media.JobUpdate{State: media.StateRunning, ProgressPercent: float32Pointer(101), LastPolledAt: started}); jobErrorKind(t, err) != media.JobErrorInvalid {
		t.Fatalf("progress above 100 was stored: %v", err)
	}
	if got := refresh(2*time.Second, media.JobUpdate{State: media.StateRunning, ProgressPercent: float32Pointer(40)}); got.State != media.StateRunning || got.ProgressPercent == nil || *got.ProgressPercent != 40 {
		t.Fatalf("running poll = %+v", got)
	}
	if got := refresh(time.Second, media.JobUpdate{State: media.StateSucceeded, ContentAvailable: true}); got.State != media.StateRunning {
		t.Fatalf("a poll older than the stored one applied: %+v", got)
	}
	if got := refresh(3*time.Second, media.JobUpdate{State: media.StateQueued, ProgressPercent: float32Pointer(10)}); got.State != media.StateRunning || *got.ProgressPercent != 40 {
		t.Fatalf("a state regression applied: %+v", got)
	}
	if got := refresh(4*time.Second, media.JobUpdate{State: media.StateRunning, ProgressPercent: float32Pointer(20)}); *got.ProgressPercent != 40 {
		t.Fatalf("progress went backwards: %+v", got)
	}
	succeeded := refresh(5*time.Second, media.JobUpdate{State: media.StateSucceeded, ProgressPercent: float32Pointer(100), ContentAvailable: true})
	if succeeded.State != media.StateSucceeded || !succeeded.ContentAvailable || succeeded.CompletedAt == nil {
		t.Fatalf("succeeded poll = %+v", succeeded)
	}
	failed := "upstream_failed"
	if got := refresh(6*time.Second, media.JobUpdate{State: media.StateFailed, ErrorClass: &failed}); got.State != media.StateSucceeded || got.ErrorClass != nil {
		t.Fatalf("a terminal job changed state: %+v", got)
	}

	pending, err := media.BeginDeletion(ctx, f.pool, reserved.ID)
	if err != nil || pending.Lifecycle != media.LifecycleDeletePending {
		t.Fatalf("begin deletion = %+v, %v", pending, err)
	}
	if again, err := media.BeginDeletion(ctx, f.pool, reserved.ID); err != nil || again.Lifecycle != media.LifecycleDeletePending {
		t.Fatalf("a repeated delete = %+v, %v", again, err)
	}
	if got := refresh(7*time.Second, media.JobUpdate{State: media.StateSucceeded}); got.Lifecycle != media.LifecycleDeletePending {
		t.Fatalf("a late poll undid delete intent: %+v", got)
	}
	if finalized, err := media.FinalizeDeletion(ctx, f.pool, reserved.ID); err != nil || !finalized {
		t.Fatalf("finalize = %v, %v", finalized, err)
	}
	if finalized, err := media.FinalizeDeletion(ctx, f.pool, reserved.ID); err != nil || finalized {
		t.Fatalf("a second finalize = %v, %v", finalized, err)
	}
	tombstone, err := media.Job(ctx, f.pool, reserved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.Lifecycle != media.LifecycleDeleted || tombstone.DeletedAt == nil || tombstone.ContentAvailable {
		t.Fatalf("tombstone = %+v", tombstone)
	}
	if again, err := media.BeginDeletion(ctx, f.pool, reserved.ID); err != nil || again.Lifecycle != media.LifecycleDeleted {
		t.Fatalf("deleting a tombstone = %+v, %v", again, err)
	}
	if _, err = media.Job(ctx, f.pool, uuid.NewString()); jobErrorKind(t, err) != media.JobErrorNotFound {
		t.Fatalf("an unknown job: %v", err)
	}
}

func TestAbandonedVideoCreatesWaitForUpstreamCleanup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)
	creating := f.seed(t, seededJob{state: media.StateQueued, lifecycle: media.LifecycleCreating})

	ambiguous, err := media.MarkCreateAmbiguous(ctx, f.pool, creating, "timeout")
	if err != nil || ambiguous.Lifecycle != media.LifecycleCreateAmbiguous || ambiguous.ReconciliationError == nil || *ambiguous.ReconciliationError != "timeout" {
		t.Fatalf("mark ambiguous = %+v, %v", ambiguous, err)
	}
	if _, err = media.MarkCreateCleanupPending(ctx, f.pool, creating, " ", "attach_failed"); jobErrorKind(t, err) != media.JobErrorInvalid {
		t.Fatalf("cleanup without an upstream identity: %v", err)
	}
	cleanup, err := media.MarkCreateCleanupPending(ctx, f.pool, creating, "vid_orphan", "attach_failed")
	if err != nil || cleanup.Lifecycle != media.LifecycleCreateCleanupPending || cleanup.UpstreamJobID == nil || *cleanup.UpstreamJobID != "vid_orphan" {
		t.Fatalf("mark cleanup = %+v, %v", cleanup, err)
	}
	if _, err = media.MarkCreateAmbiguous(ctx, f.pool, creating, "timeout"); jobErrorKind(t, err) != media.JobErrorPrecondition {
		t.Fatalf("a cleanup job went back to ambiguous: %v", err)
	}
	if _, err = media.AttachUpstream(ctx, f.pool, creating, "vid_orphan", media.JobUpdate{State: media.StateQueued, LastPolledAt: time.Now()}); jobErrorKind(t, err) != media.JobErrorPrecondition {
		t.Fatalf("a cleanup job became active: %v", err)
	}
	if _, err = media.MarkCreateAmbiguous(ctx, f.pool, uuid.NewString(), "timeout"); jobErrorKind(t, err) != media.JobErrorNotFound {
		t.Fatalf("an unknown job: %v", err)
	}
	active := f.seed(t, seededJob{state: media.StateRunning, lifecycle: media.LifecycleActive, upstream: "vid_live"})
	if _, err = media.BeginDeletion(ctx, f.pool, creating); jobErrorKind(t, err) != media.JobErrorPrecondition {
		t.Fatalf("a client delete of an unattached job: %v", err)
	}
	if finalized, err := media.FinalizeDeletion(ctx, f.pool, active); err != nil || finalized {
		t.Fatalf("an active job was tombstoned without delete intent: %v, %v", finalized, err)
	}
	if finalized, err := media.FinalizeDeletion(ctx, f.pool, creating); err != nil || !finalized {
		t.Fatalf("finalize cleanup = %v, %v", finalized, err)
	}
}

func TestVideoJobClaimsFenceConcurrentWorkers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)
	ambiguous := f.seed(t, seededJob{state: media.StateQueued, lifecycle: media.LifecycleCreateAmbiguous})
	polling := f.seed(t, seededJob{state: media.StateRunning, lifecycle: media.LifecycleActive, upstream: "vid_poll"})
	f.seed(t, seededJob{state: media.StateSucceeded, lifecycle: media.LifecycleActive, upstream: "vid_done"})
	f.seed(t, seededJob{state: media.StateQueued, lifecycle: media.LifecycleDeleted})
	f.seed(t, seededJob{state: media.StateQueued, lifecycle: media.LifecycleCreateAmbiguous, revision: f.callerRevisionID})
	now := time.Now().Add(time.Minute)

	summary, err := media.ReconciliationSummary(ctx, f.pool, now)
	if err != nil || summary.Pending != 1 || summary.OldestPendingAt == nil {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	claimed, err := media.ClaimJobs(ctx, f.pool, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].ID != ambiguous || claimed[1].ID != polling {
		t.Fatalf("claimed = %+v", claimed)
	}
	claimID := *claimed[0].ReconciliationClaimID
	if again, err := media.ClaimJobs(ctx, f.pool, now, 10); err != nil || len(again) != 0 {
		t.Fatalf("a second worker claimed leased jobs: %+v, %v", again, err)
	}
	if _, ok, err := media.ClaimJob(ctx, f.pool, ambiguous, now); err != nil || ok {
		t.Fatalf("a management refresh claimed a leased job: %v, %v", ok, err)
	}
	stranger := uuid.NewString()
	if extended, err := media.ExtendClaim(ctx, f.pool, ambiguous, stranger, now.Add(time.Minute)); err != nil || extended {
		t.Fatalf("a stranger extended the lease: %v, %v", extended, err)
	}
	if extended, err := media.ExtendClaim(ctx, f.pool, ambiguous, claimID, now.Add(time.Minute)); err != nil || !extended {
		t.Fatalf("the owner extended the lease: %v, %v", extended, err)
	}
	if err = media.FinishReconciliation(ctx, f.pool, ambiguous, stranger, now, nil); jobErrorKind(t, err) != media.JobErrorPrecondition {
		t.Fatalf("a stranger released the lease: %v", err)
	}
	failure := "upstream_timeout"
	if err = media.FinishReconciliation(ctx, f.pool, ambiguous, claimID, now, &failure); err != nil {
		t.Fatal(err)
	}
	record, err := media.Job(ctx, f.pool, ambiguous)
	if err != nil || record.ReconciliationClaimID != nil || record.ReconciliationError == nil || *record.ReconciliationError != failure || record.ReconciliationAttempts != 1 {
		t.Fatalf("released job = %+v, %v", record, err)
	}
	if summary, err = media.ReconciliationSummary(ctx, f.pool, now.Add(2*time.Minute)); err != nil || summary.Failed != 1 || summary.Stale != 1 {
		t.Fatalf("summary after a failure = %+v, %v", summary, err)
	}
	reclaimed, ok, err := media.ClaimJob(ctx, f.pool, ambiguous, now)
	if err != nil || !ok || reclaimed.ReconciliationClaimID == nil || *reclaimed.ReconciliationClaimID == claimID || reclaimed.ReconciliationAttempts != 2 {
		t.Fatalf("reclaim = %+v, %v, %v", reclaimed, ok, err)
	}
	if err = media.FinishReconciliation(ctx, f.pool, ambiguous, *reclaimed.ReconciliationClaimID, now, nil); err != nil {
		t.Fatal(err)
	}
	if record, err = media.Job(ctx, f.pool, ambiguous); err != nil || record.ReconciliationError != nil || record.ReconciliationAttempts != 0 {
		t.Fatalf("a clean release = %+v, %v", record, err)
	}
}

func TestVideoJobListingsHonorScopesAndCursors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)
	base := time.Now().Add(-time.Hour)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = f.seed(t, seededJob{state: media.StateRunning, lifecycle: media.LifecycleActive, upstream: "vid_" + uuid.NewString(), createdAt: base.Add(time.Duration(i) * time.Minute)})
	}
	f.seed(t, seededJob{state: media.StateQueued, lifecycle: media.LifecycleCreating, createdAt: base.Add(10 * time.Minute)})
	f.seed(t, seededJob{state: media.StateRunning, lifecycle: media.LifecycleActive, upstream: "vid_side", route: "side", createdAt: base.Add(11 * time.Minute)})
	foreign := f.seed(t, seededJob{state: media.StateRunning, lifecycle: media.LifecycleActive, upstream: "vid_foreign", key: f.otherKeyID, createdAt: base.Add(12 * time.Minute)})
	listed := func(page media.Page) []string {
		out := make([]string, len(page.Items))
		for i, item := range page.Items {
			out[i] = item.ID
		}
		return out
	}
	same := func(what string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", what, got, want)
			}
		}
	}

	all, err := media.Jobs(ctx, f.pool, media.Filters{AllProjects: true}, nil, 50)
	if err != nil || len(all.Items) != 7 || all.Items[0].ID != foreign || all.NextCursor != nil {
		t.Fatalf("all projects = %d items, %v", len(all.Items), err)
	}
	scoped, err := media.Jobs(ctx, f.pool, media.Filters{AllowedProjects: []string{f.otherProjectID}}, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	same("one project", listed(scoped), foreign)
	if denied, err := media.Jobs(ctx, f.pool, media.Filters{AllProjects: true, RouteSlugs: []string{}}, nil, 50); err != nil || len(denied.Items) != 0 {
		t.Fatalf("an empty route allowlist = %d items, %v", len(denied.Items), err)
	}
	route := "side"
	if side, err := media.Jobs(ctx, f.pool, media.Filters{AllProjects: true, RouteSlug: &route}, nil, 50); err != nil || len(side.Items) != 1 {
		t.Fatalf("one route = %d items, %v", len(side.Items), err)
	}
	lifecycle := media.LifecycleCreating
	if creating, err := media.Jobs(ctx, f.pool, media.Filters{AllProjects: true, Lifecycle: &lifecycle}, nil, 50); err != nil || len(creating.Items) != 1 {
		t.Fatalf("one lifecycle = %d items, %v", len(creating.Items), err)
	}

	key, studio := f.keyID, []string{"studio"}
	filters := media.Filters{AllProjects: true, APIKeyID: &key, RouteSlugs: studio}
	first, err := media.Jobs(ctx, f.pool, filters, nil, 3)
	if err != nil || first.NextCursor == nil {
		t.Fatalf("first management page = %+v, %v", first, err)
	}
	cursor, err := media.DecodeCursor(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := media.Jobs(ctx, f.pool, filters, cursor, 3)
	if err != nil || second.NextCursor != nil {
		t.Fatalf("second management page = %+v, %v", second, err)
	}
	same("management pages", append(listed(first), listed(second)...), append([]string{listed(first)[0]}, ids[3], ids[2], ids[1], ids[0])...)

	client := media.Filters{APIKeyID: &key, RouteSlugs: studio}
	newest, err := media.JobsAfterID(ctx, f.pool, client, nil, media.OrderDescending, 2)
	if err != nil || newest.NextCursor == nil {
		t.Fatalf("first client page = %+v, %v", newest, err)
	}
	same("newest client page", listed(newest), ids[3], ids[2])
	older, err := media.JobsAfterID(ctx, f.pool, client, newest.NextCursor, media.OrderDescending, 2)
	if err != nil || older.NextCursor != nil {
		t.Fatalf("second client page = %+v, %v", older, err)
	}
	same("older client page", listed(older), ids[1], ids[0])
	oldest, err := media.JobsAfterID(ctx, f.pool, client, &ids[1], media.OrderAscending, 10)
	if err != nil {
		t.Fatal(err)
	}
	same("ascending client page", listed(oldest), ids[2], ids[3])
	if _, err = media.JobsAfterID(ctx, f.pool, client, &foreign, media.OrderDescending, 10); jobErrorKind(t, err) != media.JobErrorInvalid {
		t.Fatalf("another key's job worked as a cursor: %v", err)
	}
}

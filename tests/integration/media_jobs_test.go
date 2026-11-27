//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
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
	state     jobs.State
	lifecycle jobs.Lifecycle
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

func (f videoFixture) reservation() jobs.Reservation {
	return jobs.Reservation{
		ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID,
		APIKeyID: f.keyID, ProviderID: f.providerID, UpstreamModel: videoModel, RouteSlug: "studio",
		Surface: "openai", CredentialVersionID: &f.credentialID, SlotID: f.slotID,
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

func jobKind(t *testing.T, err error) jobs.Kind {
	t.Helper()
	kind, ok := jobs.KindOf(err)
	if !ok {
		t.Fatalf("error %v is not a media job error", err)
	}
	return kind
}

func observed(t *testing.T, at time.Time, video media.VideoJobResult) jobs.Update {
	t.Helper()
	update, ok := jobs.Observe(&video, at)
	if !ok {
		t.Fatalf("the poll %+v was refused", video)
	}
	return update
}

func float32Pointer(value float32) *float32 { return &value }

func TestVideoJobLifecycleRunsFromReservationToTombstone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)
	store := jobs.NewStore(f.pool)

	stray := f.reservation()
	stray.UpstreamModel = "not-certified"
	if _, err := store.Reserve(ctx, stray); jobKind(t, err) != jobs.KindInvalid {
		t.Fatalf("a model outside the pinned release reserved a job: %v", err)
	}
	reserved, err := store.Reserve(ctx, f.reservation())
	if err != nil {
		t.Fatal(err)
	}
	if reserved.Lifecycle != jobs.Creating || reserved.State != jobs.Queued || reserved.UpstreamJobID != nil ||
		reserved.ProviderName != "video" || reserved.ProjectID == nil || *reserved.ProjectID != f.projectID {
		t.Fatalf("reserved job = %+v", reserved)
	}

	started := time.Now().UTC()
	queued := observed(t, started, media.VideoJobResult{Status: "queued"})
	if _, err = store.Attach(ctx, reserved.ID, " vid_1", queued); jobKind(t, err) != jobs.KindInvalid {
		t.Fatalf("a padded upstream identity attached: %v", err)
	}
	attached, err := store.Attach(ctx, reserved.ID, "vid_1", queued)
	if err != nil {
		t.Fatal(err)
	}
	if attached.Lifecycle != jobs.Active || attached.UpstreamJobID == nil || *attached.UpstreamJobID != "vid_1" || attached.ETag == reserved.ETag {
		t.Fatalf("attached job = %+v", attached)
	}
	if retried, err := store.Attach(ctx, reserved.ID, "vid_1", queued); err != nil || retried.ID != reserved.ID {
		t.Fatalf("an ambiguous attach retry with the same identity: %+v, %v", retried, err)
	}
	if _, err = store.Attach(ctx, reserved.ID, "vid_2", queued); jobKind(t, err) != jobs.KindChanged {
		t.Fatalf("an attached job took a second identity: %v", err)
	}
	twin, err := store.Reserve(ctx, f.reservation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Attach(ctx, twin.ID, "vid_1", queued); jobKind(t, err) != jobs.KindConflict {
		t.Fatalf("two jobs share one upstream identity: %v", err)
	}

	if _, ok := jobs.Observe(&media.VideoJobResult{Status: "in_progress", Progress: float32Pointer(101)}, started); ok {
		t.Fatal("a poll with progress above 100 was accepted")
	}
	refresh := func(at time.Duration, video media.VideoJobResult) jobs.Record {
		t.Helper()
		record, err := store.Refresh(ctx, reserved.ID, jobs.Unfenced, observed(t, started.Add(at), video))
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	if got := refresh(2*time.Second, media.VideoJobResult{Status: "in_progress", Progress: float32Pointer(40)}); got.State != jobs.Running || got.ProgressPercent == nil || *got.ProgressPercent != 40 {
		t.Fatalf("running poll = %+v", got)
	}
	if got := refresh(time.Second, media.VideoJobResult{Status: "completed"}); got.State != jobs.Running {
		t.Fatalf("a poll older than the stored one applied: %+v", got)
	}
	if got := refresh(3*time.Second, media.VideoJobResult{Status: "queued", Progress: float32Pointer(10)}); got.State != jobs.Running || *got.ProgressPercent != 40 {
		t.Fatalf("a state regression applied: %+v", got)
	}
	if got := refresh(4*time.Second, media.VideoJobResult{Status: "in_progress", Progress: float32Pointer(20)}); *got.ProgressPercent != 40 {
		t.Fatalf("progress went backwards: %+v", got)
	}
	succeeded := refresh(5*time.Second, media.VideoJobResult{Status: "completed", Progress: float32Pointer(100)})
	if succeeded.State != jobs.Succeeded || !succeeded.ContentAvailable() || succeeded.CompletedAt == nil {
		t.Fatalf("succeeded poll = %+v", succeeded)
	}
	failed := "upstream_failed"
	if got := refresh(6*time.Second, media.VideoJobResult{Status: "failed", ErrorCode: &failed}); got.State != jobs.Succeeded || got.ErrorClass != nil {
		t.Fatalf("a terminal job changed state: %+v", got)
	}

	pending, err := store.BeginDeletion(ctx, reserved.ID, jobs.Unfenced)
	if err != nil || pending.Lifecycle != jobs.DeletePending {
		t.Fatalf("begin deletion = %+v, %v", pending, err)
	}
	if again, err := store.BeginDeletion(ctx, reserved.ID, jobs.Unfenced); err != nil || again.Lifecycle != jobs.DeletePending {
		t.Fatalf("a repeated delete = %+v, %v", again, err)
	}
	if got := refresh(7*time.Second, media.VideoJobResult{Status: "completed"}); got.Lifecycle != jobs.DeletePending {
		t.Fatalf("a late poll undid delete intent: %+v", got)
	}
	tombstone, err := store.Finalize(ctx, reserved.ID, jobs.Unfenced)
	if err != nil || tombstone.Lifecycle != jobs.Deleted || tombstone.DeletedAt == nil || tombstone.ContentAvailable() {
		t.Fatalf("tombstone = %+v, %v", tombstone, err)
	}
	if again, err := store.Finalize(ctx, reserved.ID, jobs.Unfenced); err != nil || again.Lifecycle != jobs.Deleted || !again.DeletedAt.Equal(*tombstone.DeletedAt) {
		t.Fatalf("a second finalize = %+v, %v", again, err)
	}
	if again, err := store.BeginDeletion(ctx, reserved.ID, jobs.Unfenced); err != nil || again.Lifecycle != jobs.Deleted {
		t.Fatalf("deleting a tombstone = %+v, %v", again, err)
	}
	if _, err = store.Get(ctx, uuid.NewString()); jobKind(t, err) != jobs.KindNotFound {
		t.Fatalf("an unknown job: %v", err)
	}
}

func TestAbandonedVideoCreatesWaitForUpstreamCleanup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)
	store := jobs.NewStore(f.pool)
	creating := f.seed(t, seededJob{state: jobs.Queued, lifecycle: jobs.Creating})

	ambiguous, err := store.MarkAmbiguous(ctx, creating, jobs.Unfenced, "timeout")
	if err != nil || ambiguous.Lifecycle != jobs.CreateAmbiguous || ambiguous.ReconciliationError == nil || *ambiguous.ReconciliationError != "timeout" {
		t.Fatalf("mark ambiguous = %+v, %v", ambiguous, err)
	}
	if _, err = store.MarkCleanup(ctx, creating, jobs.Unfenced, " ", "attach_failed"); jobKind(t, err) != jobs.KindInvalid {
		t.Fatalf("cleanup without an upstream identity: %v", err)
	}
	cleanup, err := store.MarkCleanup(ctx, creating, jobs.Unfenced, "vid_orphan", "attach_failed")
	if err != nil || cleanup.Lifecycle != jobs.CleanupPending || cleanup.UpstreamJobID == nil || *cleanup.UpstreamJobID != "vid_orphan" {
		t.Fatalf("mark cleanup = %+v, %v", cleanup, err)
	}
	if _, err = store.MarkAmbiguous(ctx, creating, jobs.Unfenced, "timeout"); jobKind(t, err) != jobs.KindChanged {
		t.Fatalf("a cleanup job went back to ambiguous: %v", err)
	}
	if _, err = store.Attach(ctx, creating, "vid_orphan", observed(t, time.Now(), media.VideoJobResult{Status: "queued"})); jobKind(t, err) != jobs.KindChanged {
		t.Fatalf("a cleanup job became active: %v", err)
	}
	if _, err = store.MarkAmbiguous(ctx, uuid.NewString(), jobs.Unfenced, "timeout"); jobKind(t, err) != jobs.KindNotFound {
		t.Fatalf("an unknown job: %v", err)
	}
	active := f.seed(t, seededJob{state: jobs.Running, lifecycle: jobs.Active, upstream: "vid_live"})
	if _, err = store.BeginDeletion(ctx, creating, jobs.Unfenced); jobKind(t, err) != jobs.KindChanged {
		t.Fatalf("a client delete of an unattached job: %v", err)
	}
	if kept, err := store.Finalize(ctx, active, jobs.Unfenced); err != nil || kept.Lifecycle != jobs.Active {
		t.Fatalf("an active job was tombstoned without delete intent: %+v, %v", kept, err)
	}
	if finalized, err := store.Finalize(ctx, creating, jobs.Unfenced); err != nil || finalized.Lifecycle != jobs.Deleted {
		t.Fatalf("finalize cleanup = %+v, %v", finalized, err)
	}
}

func TestVideoJobClaimsFenceConcurrentWorkers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)
	store := jobs.NewStore(f.pool)
	ambiguous := f.seed(t, seededJob{state: jobs.Queued, lifecycle: jobs.CreateAmbiguous})
	polling := f.seed(t, seededJob{state: jobs.Running, lifecycle: jobs.Active, upstream: "vid_poll"})
	f.seed(t, seededJob{state: jobs.Succeeded, lifecycle: jobs.Active, upstream: "vid_done"})
	f.seed(t, seededJob{state: jobs.Queued, lifecycle: jobs.Deleted})
	caller := f.seed(t, seededJob{state: jobs.Queued, lifecycle: jobs.CreateAmbiguous, revision: f.callerRevisionID})
	now := time.Now().Add(time.Minute)

	summary, err := store.Summary(ctx, now)
	if err != nil || summary.Pending != 1 || summary.OldestPendingAt == nil {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	claimed, err := store.Claim(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].ID != ambiguous || claimed[1].ID != polling {
		t.Fatalf("claimed = %+v", claimed)
	}
	if again, err := store.Claim(ctx, now, 10); err != nil || len(again) != 0 {
		t.Fatalf("a second worker claimed leased jobs: %+v, %v", again, err)
	}
	if _, ok, err := store.ClaimOne(ctx, ambiguous, now); err != nil || ok {
		t.Fatalf("a management refresh claimed a leased job: %v, %v", ok, err)
	}
	operator := jobs.New(jobs.Deps{Store: store, Now: func() time.Time { return now }})
	if _, err := operator.Refresh(ctx, caller); jobKind(t, err) != jobs.KindCallerCredential {
		t.Fatalf("a management refresh of a caller-credential job = %v", err)
	}
	if _, err := operator.Delete(ctx, caller); jobKind(t, err) != jobs.KindCallerCredential {
		t.Fatalf("a management delete of a caller-credential job = %v", err)
	}
	if record, err := store.Get(ctx, caller); err != nil || record.Lifecycle != jobs.CreateAmbiguous || record.ReconciliationAttempts != 0 {
		t.Fatalf("refused management calls changed the caller-credential job: %+v, %v", record, err)
	}
	first := claimed[0]
	if extended, err := store.Extend(ctx, first, now.Add(time.Minute)); err != nil || !extended {
		t.Fatalf("the owner extended the lease: %v, %v", extended, err)
	}
	failure := "upstream_timeout"
	if err = store.Finish(ctx, first, now, &failure); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(ctx, ambiguous)
	if err != nil || record.ReconciliationClaimID != nil || record.ReconciliationError == nil || *record.ReconciliationError != failure || record.ReconciliationAttempts != 1 {
		t.Fatalf("released job = %+v, %v", record, err)
	}
	if summary, err = store.Summary(ctx, now.Add(2*time.Minute)); err != nil || summary.Failed != 1 || summary.Stale != 1 {
		t.Fatalf("summary after a failure = %+v, %v", summary, err)
	}
	reclaimed, ok, err := store.ClaimOne(ctx, ambiguous, now)
	if err != nil || !ok || reclaimed.ReconciliationClaimID == nil || *reclaimed.ReconciliationClaimID == *first.ReconciliationClaimID || reclaimed.ReconciliationAttempts != 2 {
		t.Fatalf("reclaim = %+v, %v, %v", reclaimed, ok, err)
	}
	if extended, err := store.Extend(ctx, first, now.Add(time.Hour)); err != nil || extended {
		t.Fatalf("a stale claim extended the new lease: %v, %v", extended, err)
	}
	if _, err = store.MarkCleanup(ctx, ambiguous, first.Fence(), "vid_stale", "stale"); err == nil {
		t.Fatal("a stale claim moved the job")
	}
	if err = store.Finish(ctx, first, now, nil); jobKind(t, err) != jobs.KindChanged {
		t.Fatalf("a stale claim released the new lease: %v", err)
	}
	if err = store.Finish(ctx, reclaimed, now, nil); err != nil {
		t.Fatal(err)
	}
	if record, err = store.Get(ctx, ambiguous); err != nil || record.Lifecycle != jobs.CreateAmbiguous || record.ReconciliationError != nil || record.ReconciliationAttempts != 0 {
		t.Fatalf("a clean release = %+v, %v", record, err)
	}
}

func TestVideoJobListingsHonorScopesAndCursors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newVideoFixture(t)
	store := jobs.NewStore(f.pool)
	base := time.Now().Add(-time.Hour)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = f.seed(t, seededJob{state: jobs.Running, lifecycle: jobs.Active, upstream: "vid_" + uuid.NewString(), createdAt: base.Add(time.Duration(i) * time.Minute)})
	}
	f.seed(t, seededJob{state: jobs.Queued, lifecycle: jobs.Creating, createdAt: base.Add(10 * time.Minute)})
	f.seed(t, seededJob{state: jobs.Running, lifecycle: jobs.Active, upstream: "vid_side", route: "side", createdAt: base.Add(11 * time.Minute)})
	foreign := f.seed(t, seededJob{state: jobs.Running, lifecycle: jobs.Active, upstream: "vid_foreign", key: f.otherKeyID, createdAt: base.Add(12 * time.Minute)})
	list := func(q jobs.Query) ([]string, bool) {
		t.Helper()
		page, err := store.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(page.Items))
		for i, item := range page.Items {
			out[i] = item.ID
		}
		return out, page.More
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

	if all, more := list(jobs.Query{Limit: 50}); len(all) != 7 || all[0] != foreign || more {
		t.Fatalf("all projects = %v, %v", all, more)
	}
	scoped, _ := list(jobs.Query{Projects: jobs.Only([]string{f.otherProjectID}), Limit: 50})
	same("one project", scoped, foreign)
	if denied, _ := list(jobs.Query{Routes: jobs.Only(nil), Limit: 50}); len(denied) != 0 {
		t.Fatalf("an empty route allowlist = %v", denied)
	}
	if side, _ := list(jobs.Query{Routes: jobs.Only([]string{"side"}), Limit: 50}); len(side) != 1 {
		t.Fatalf("one route = %v", side)
	}
	lifecycle := jobs.Creating
	if creating, _ := list(jobs.Query{Lifecycle: &lifecycle, Limit: 50}); len(creating) != 1 {
		t.Fatalf("one lifecycle = %v", creating)
	}

	key, studio := f.keyID, jobs.Only([]string{"studio"})
	page, err := store.List(ctx, jobs.Query{APIKeyID: &key, Routes: studio, Limit: 3})
	if err != nil || !page.More {
		t.Fatalf("first management page = %+v, %v", page, err)
	}
	last := page.Items[len(page.Items)-1]
	second, more := list(jobs.Query{APIKeyID: &key, Routes: studio, Limit: 3, After: &jobs.Position{CreatedAt: last.CreatedAt, ID: last.ID}})
	if more {
		t.Fatal("the second management page reported more")
	}
	same("management pages", append([]string{page.Items[0].ID, page.Items[1].ID, last.ID}, second...), page.Items[0].ID, ids[3], ids[2], ids[1], ids[0])

	active := jobs.Active
	client := jobs.Query{APIKeyID: &key, Routes: studio, Lifecycle: &active, Order: media.OrderDescending, Limit: 2}
	newest, more := list(client)
	if !more {
		t.Fatal("the first client page reported no more")
	}
	same("newest client page", newest, ids[3], ids[2])
	after := func(id string, q jobs.Query) jobs.Query {
		t.Helper()
		position, err := store.Position(ctx, &id, q)
		if err != nil {
			t.Fatal(err)
		}
		q.After = position
		return q
	}
	older, more := list(after(newest[1], client))
	if more {
		t.Fatal("the second client page reported more")
	}
	same("older client page", older, ids[1], ids[0])
	ascending := client
	ascending.Order, ascending.Limit = media.OrderAscending, 10
	oldest, _ := list(after(ids[1], ascending))
	same("ascending client page", oldest, ids[2], ids[3])
	if _, err = store.Position(ctx, &foreign, client); jobKind(t, err) != jobs.KindInvalid {
		t.Fatalf("another key's job worked as a cursor: %v", err)
	}
}

func videoObjects(issues string) map[string]string {
	replies := map[string]string{"POST /videos": providerVideo(issues, "queued")}
	for _, n := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"} {
		replies["GET /videos/video_"+providerOwned+n] = providerVideo(n, "completed")
		replies["GET /videos/video_"+providerOwned+n+"/content"] = "MP4DATA"
		replies["DELETE /videos/video_"+providerOwned+n] = `{"id":"video_` + providerOwned + n + `","object":"video.deleted","deleted":true}`
	}
	return replies
}

func providerVideo(n, status string) string {
	return `{"id":"video_` + providerOwned + n + `","object":"video","status":"` + status + `","model":"video-model","created_at":1,"seconds":"4","size":"720x1280","progress":0}`
}

type videoReply struct {
	ID, Status, Model string
	Deleted           bool
	HasMore           bool   `json:"has_more"`
	FirstID           string `json:"first_id"`
	LastID            string `json:"last_id"`
	Data              []struct{ ID, Status string }
	Error             struct{ Code string }
	raw               string
	contentType       string
}

func (r videoReply) listed() []string {
	ids := make([]string, len(r.Data))
	for i, item := range r.Data {
		ids[i] = item.ID
	}
	return ids
}

func videoForm(fields ...string) (string, string) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for i := 0; i+1 < len(fields); i += 2 {
		_ = form.WriteField(fields[i], fields[i+1])
	}
	_ = form.Close()
	return form.FormDataContentType(), body.String()
}

func TestVideoJobsHideProviderIdentifiersPageNewestFirstIsolateKeysAndKeepOrForgetWhatEachProviderAnswerDecides(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "studio", "video-model"
	provider := &storeProvider{issues: "1", objects: videoObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status, headers: func(r *http.Request) map[string]string {
		if strings.HasSuffix(r.URL.Path, "/content") {
			return map[string]string{"Content-Type": "video/mp4"}
		}
		return nil
	}}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{slug: slug, model: model, kind: "openai_compatible", sink: sink,
		operations: []string{"video_create", "video_get", "video_content", "video_delete", "video_list"}, upstream: upstream})
	g.key(t, "owner")
	g.key(t, "stranger")
	providerVideoPath := func(n string) string { return "/videos/video_" + providerOwned + n }
	asked := func() int {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		return len(upstream.calls)
	}
	ask := func(name, key, method, path string, fields []string, status int, code string, attempts ...string) videoReply {
		t.Helper()
		settled := len(sink.all())
		contentType, body := "", ""
		if fields != nil {
			contentType, body = videoForm(fields...)
		}
		header, raw := g.send(t, method, path, map[string]string{"Authorization": "Bearer " + key, "Content-Type": contentType}, body, status)
		reply := videoReply{raw: string(raw), contentType: header.Get("Content-Type")}
		_ = json.Unmarshal(raw, &reply)
		if reply.Error.Code != code {
			t.Fatalf("%s: answered %s, want code %q", name, raw, code)
		}
		harness.Eventually(t, name+" is recorded", time.Second, func() (bool, string) { return len(sink.all()) > settled, "no terminal record" })
		seen := []string{}
		for _, attempt := range sink.all()[settled].Attempts {
			seen = append(seen, fmt.Sprintf("%s committed=%v uncertain=%v", attempt.Class, attempt.Committed, attempt.BillingUncertain))
		}
		if !slices.Equal(seen, attempts) {
			t.Fatalf("%s: provider attempts %v, want %v", name, seen, attempts)
		}
		return reply
	}
	unasked := func(name, key, method, path string, fields []string, status int, code string) videoReply {
		t.Helper()
		before := asked()
		reply := ask(name, key, method, path, fields, status, code)
		if asked() != before {
			t.Fatalf("%s: the provider was asked", name)
		}
		return reply
	}
	kept := func() []string {
		t.Helper()
		rows, err := pool.Query(t.Context(), `SELECT lifecycle_state||' '||state||' '||coalesce(upstream_job_id,'unnamed') FROM olp.media_jobs ORDER BY created_at, id`)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	job := func(n, lifecycle, state string) string {
		return lifecycle + " " + state + " video_" + providerOwned + n
	}
	prompt := []string{"model", slug, "prompt", "a cat", "seconds", "4"}

	for _, refusal := range []struct {
		name, key, path string
		fields          []string
		status          int
		code            string
	}{
		{"a creation without a prompt", "owner", "/v1/videos", []string{"model", slug, "seconds", "4"}, 400, "invalid_request"},
		{"a creation of an unsupported length", "owner", "/v1/videos", []string{"model", slug, "prompt", "a cat", "seconds", "5"}, 400, "invalid_request"},
		{"a creation on an unknown route", "owner", "/v1/videos", []string{"model", "elsewhere", "prompt", "a cat"}, 404, "route_not_found"},
	} {
		unasked(refusal.name, refusal.key, http.MethodPost, refusal.path, refusal.fields, refusal.status, refusal.code)
	}
	if jobs := kept(); len(jobs) != 0 {
		t.Fatalf("refused creations kept %v", jobs)
	}

	first := ask("a creation", "owner", http.MethodPost, "/v1/videos", prompt, 201, "", "success committed=true uncertain=false")
	if _, err := uuid.Parse(first.ID); err != nil || first.Model != slug || first.Status != "queued" {
		t.Fatalf("created video = %s, want a gateway identifier under the route name", first.raw)
	}
	creation := upstream.sent("POST /videos")
	if len(creation) != 1 || creation[0].authorization != "Bearer listed-secret" || !strings.Contains(creation[0].body, model) || strings.Contains(creation[0].body, slug) || !strings.Contains(creation[0].body, "a cat") {
		t.Fatalf("provider creation = %+v, want the provider model and the prompt under the provider credential", creation)
	}

	for _, broken := range []struct {
		name, body string
		provider   int
		status     int
		code       string
		attempt    string
	}{
		{"a creation the provider refuses", `{"error":{"message":"no","type":"invalid_request_error"}}`, 400, 400, "upstream_rejected", "upstream_client committed=false uncertain=false"},
		{"a creation the provider fails", `{"error":{"message":"down"}}`, 500, 502, "ambiguous_upstream_result", "ambiguous committed=false uncertain=true"},
		{"a creation the provider answers without a job", `{"object":"video","status":"queued"}`, 200, 502, "provider_protocol_error", "success committed=false uncertain=true"},
		{"a creation the provider answers with a list", `[]`, 200, 502, "ambiguous_upstream_result", "ambiguous committed=false uncertain=true"},
	} {
		provider.answers("POST /videos", broken.provider, broken.body)
		ask(broken.name, "owner", http.MethodPost, "/v1/videos", prompt, broken.status, broken.code, broken.attempt)
	}
	provider.recovers("POST /videos")
	unsettled := []string{job("1", "active", "queued"), "deleted queued unnamed", "create_ambiguous queued unnamed", "create_ambiguous queued unnamed", "create_ambiguous queued unnamed"}
	if jobs := kept(); !slices.Equal(jobs, unsettled) {
		t.Fatalf("after one accepted, one refused and three uncertain creations the gateway keeps %v, want %v", jobs, unsettled)
	}

	provider.creates("2")
	second := ask("a second creation", "owner", http.MethodPost, "/v1/videos", prompt, 201, "", "success committed=true uncertain=false")
	provider.creates("3")
	third := ask("a third creation", "owner", http.MethodPost, "/v1/videos", prompt, 201, "", "success committed=true uncertain=false")

	refreshed := "success committed=true uncertain=false"
	listed := ask("a list of unfinished videos", "owner", http.MethodGet, "/v1/videos", nil, 200, "", refreshed, refreshed, refreshed)
	if want := []string{third.ID, second.ID, first.ID}; !slices.Equal(listed.listed(), want) || listed.HasMore || listed.Data[0].Status != "completed" {
		t.Fatalf("list = %s, want the accepted videos %v newest first, each asked of the provider and now completed", listed.raw, want)
	}
	for _, page := range []struct {
		name, query string
		want        []string
		more        bool
	}{
		{"the first page", "?limit=1", []string{third.ID}, true},
		{"the page after a cursor", "?limit=1&after=" + third.ID, []string{second.ID}, true},
		{"the oldest first", "?order=asc", []string{first.ID, second.ID, third.ID}, false},
	} {
		got := ask(page.name, "owner", http.MethodGet, "/v1/videos"+page.query, nil, 200, "")
		if !slices.Equal(got.listed(), page.want) || got.HasMore != page.more || got.FirstID != page.want[0] || got.LastID != page.want[len(page.want)-1] {
			t.Fatalf("%s = %s, want %v with more=%v", page.name, got.raw, page.want, page.more)
		}
	}
	for _, refusal := range []struct{ name, method, key, path, code string }{
		{"a list in an unknown order", http.MethodGet, "owner", "/v1/videos?order=up", "invalid_request"},
		{"a list of no videos", http.MethodGet, "owner", "/v1/videos?limit=0", "invalid_request"},
		{"a list after a cursor that is no identifier", http.MethodGet, "owner", "/v1/videos?after=nope", "invalid_request"},
		{"a list after a cursor that names no video", http.MethodGet, "owner", "/v1/videos?after=" + uuid.NewString(), "invalid_request"},
		{"a list with an unknown parameter", http.MethodGet, "owner", "/v1/videos?color=red", "invalid_request"},
		{"a content variant the gateway does not know", http.MethodGet, "owner", "/v1/videos/" + second.ID + "/content?variant=nope", "invalid_request"},
	} {
		unasked(refusal.name, refusal.key, refusal.method, refusal.path, nil, 400, refusal.code)
	}
	for _, hidden := range []struct{ name, method, key, path, code string }{
		{"another key's read", http.MethodGet, "stranger", "/v1/videos/" + first.ID, "video_not_found"},
		{"another key's download", http.MethodGet, "stranger", "/v1/videos/" + first.ID + "/content", "video_not_found"},
		{"another key's delete", http.MethodDelete, "stranger", "/v1/videos/" + first.ID, "video_not_found"},
		{"a read of no identifier", http.MethodGet, "owner", "/v1/videos/nope", "video_not_found"},
		{"a read of an unknown video", http.MethodGet, "owner", "/v1/videos/" + uuid.NewString(), "media_job_not_found"},
	} {
		unasked(hidden.name, hidden.key, hidden.method, hidden.path, nil, 404, hidden.code)
	}
	if strangers := unasked("another key's list", "stranger", http.MethodGet, "/v1/videos", nil, 200, ""); len(strangers.Data) != 0 {
		t.Fatalf("another key lists %s", strangers.raw)
	}

	read := ask("a read", "owner", http.MethodGet, "/v1/videos/"+first.ID, nil, 200, "", "success committed=true uncertain=false")
	if read.ID != first.ID || read.Model != slug || read.Status != "completed" {
		t.Fatalf("read = %s, want the completed video under its gateway identifier and route name", read.raw)
	}
	for _, broken := range []struct {
		name, body string
		provider   int
		status     int
		code       string
		attempt    string
	}{
		{"a read the provider fails", `{"error":{"message":"down"}}`, 500, 502, "upstream_unavailable", "upstream_server committed=false uncertain=true"},
		{"a read the provider cannot find", `{"error":{"message":"gone"}}`, 404, 404, "upstream_rejected", "upstream_client committed=false uncertain=false"},
		{"a read the provider answers with a list", `[]`, 200, 502, "provider_protocol_error", "protocol committed=false uncertain=true"},
	} {
		provider.answers("GET "+providerVideoPath("1"), broken.provider, broken.body)
		ask(broken.name, "owner", http.MethodGet, "/v1/videos/"+first.ID, nil, broken.status, broken.code, broken.attempt)
	}
	provider.recovers("GET " + providerVideoPath("1"))

	thumbnail := ask("a download", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content?variant=thumbnail", nil, 200, "", "success committed=true uncertain=false")
	downloads := upstream.sent("GET " + providerVideoPath("2") + "/content")
	if thumbnail.raw != "MP4DATA" || thumbnail.contentType != "video/mp4" || len(downloads) != 1 || downloads[0].query != "variant=thumbnail" {
		t.Fatalf("download = %q as %q through %+v, want the provider's bytes, type and the asked variant", thumbnail.raw, thumbnail.contentType, downloads)
	}
	provider.answers("GET "+providerVideoPath("2")+"/content", 404, `{"error":{"message":"gone"}}`)
	ask("a download the provider cannot find", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content", nil, 404, "upstream_rejected", "upstream_client committed=false uncertain=false")
	provider.answers("GET "+providerVideoPath("2")+"/content", 500, `{"error":{"message":"down"}}`)
	ask("a download the provider fails", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content", nil, 502, "upstream_unavailable", "upstream_server committed=false uncertain=true")

	provider.answers("DELETE "+providerVideoPath("2"), 500, `{"error":{"message":"down"}}`)
	ask("a delete the provider fails", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 502, "ambiguous_upstream_result", "ambiguous committed=false uncertain=true")
	provider.answers("DELETE "+providerVideoPath("2"), 200, `{"id":"video_`+providerOwned+`2","object":"video.deleted","deleted":false}`)
	ask("a delete the provider declines", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 502, "upstream_rejected", "success committed=true uncertain=false")
	if still := ask("a read after two failed deletes", "owner", http.MethodGet, "/v1/videos/"+second.ID, nil, 200, "", "success committed=true uncertain=false"); still.ID != second.ID {
		t.Fatalf("a video whose delete failed reads as %s", still.raw)
	}
	provider.answers("DELETE "+providerVideoPath("2"), 404, `{"error":{"message":"gone"}}`)
	gone := ask("a delete the provider has already forgotten", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 200, "", "upstream_client committed=false uncertain=false")
	again := unasked("a repeated delete", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 200, "")
	if !gone.Deleted || gone.ID != second.ID || !again.Deleted || again.ID != second.ID {
		t.Fatalf("deletes = %s then %s, want the gateway identifier deleted both times", gone.raw, again.raw)
	}
	unasked("a read of a deleted video", "owner", http.MethodGet, "/v1/videos/"+second.ID, nil, 404, "video_not_found")
	unasked("a download of a deleted video", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content", nil, 404, "video_not_found")
	if removed := ask("a delete", "owner", http.MethodDelete, "/v1/videos/"+third.ID, nil, 200, "", "success committed=true uncertain=false"); !removed.Deleted || len(upstream.sent("DELETE "+providerVideoPath("3"))) != 1 {
		t.Fatalf("delete = %s, want one provider delete", removed.raw)
	}
	if left := ask("a list after the deletes", "owner", http.MethodGet, "/v1/videos", nil, 200, ""); !slices.Equal(left.listed(), []string{first.ID}) {
		t.Fatalf("list after the deletes = %s, want only %s", left.raw, first.ID)
	}
	settled := append(slices.Clone(unsettled[1:]), job("1", "active", "succeeded"), job("2", "deleted", "succeeded"), job("3", "deleted", "succeeded"))
	slices.Sort(settled)
	jobs := kept()
	slices.Sort(jobs)
	if !slices.Equal(jobs, settled) {
		t.Fatalf("the gateway keeps %v, want %v", jobs, settled)
	}
}

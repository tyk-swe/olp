package manage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
)

func TestContentFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		contentType string
		wantSuffix  string
	}{
		{"video/mp4", ".mp4"},
		{"VIDEO/MP4; codecs=avc1", ".mp4"},
		{"image/png", ".png"},
		{"image/jpeg", ".jpg"},
		{"image/webp", ".webp"},
		{"image/gif", ".gif"},
		{"application/octet-stream", ".bin"},
		{"", ".bin"},
	}
	for _, tc := range cases {
		got := contentFilename("job-1", "thumbnail", tc.contentType)
		want := "olp-media-job-1-thumbnail" + tc.wantSuffix
		if got != want {
			t.Errorf("contentFilename(%q) = %q, want %q", tc.contentType, got, want)
		}
	}
}

func TestCursorRoundTripAndMalformed(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("UTC+2", 2*3600)
	created := time.Date(2026, 4, 5, 14, 30, 15, 123456789, loc)
	id := uuid.MustParse("019605f0-1234-7abc-8def-0123456789ab")

	if got, err := decodeCursor(""); err != nil || got != nil {
		t.Fatalf("decodeCursor(\"\") = (%+v, %v), want (nil, nil)", got, err)
	}

	encoded := encodeCursor(jobs.Record{CreatedAt: created, ID: id.String()})
	decoded, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("decodeCursor(%q) unexpected error: %v", encoded, err)
	}
	if !decoded.CreatedAt.Equal(created) || decoded.ID != id.String() {
		t.Fatalf("decodeCursor(%q) = %+v", encoded, decoded)
	}

	malformed := []string{
		"no-pipe",
		"not-a-time|" + id.String(),
		created.UTC().Format(time.RFC3339Nano) + "|not-a-uuid",
		created.UTC().Format(time.RFC3339Nano) + "|",
		"|" + id.String(),
		created.UTC().Format(time.RFC3339Nano) + "|" + id.String() + "|extra",
	}
	for _, raw := range malformed {
		_, err := decodeCursor(raw)
		p, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || p.Status != http.StatusBadRequest || p.Code != "invalid_cursor" {
			t.Errorf("decodeCursor(%q) = %v, want 400 invalid_cursor", raw, err)
		}
	}
}

func TestParseListQueryAndValidation(t *testing.T) {
	t.Parallel()
	projectID := uuid.NewString()
	scopedPrincipal := authz.Principal{AllProjects: false, Projects: map[string]string{projectID: "viewer"}}
	globalPrincipal := authz.Principal{AllProjects: true}

	invalidCases := []struct {
		name       string
		params     url.Values
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{"invalid api_key_id", url.Values{"api_key_id": {"bad"}}, 422, "validation_failed", "api_key_id"},
		{"invalid provider_id", url.Values{"provider_id": {"123"}}, 422, "validation_failed", "provider_id"},
		{"invalid state", url.Values{"state": {"completed"}}, 422, "validation_failed", "state"},
		{"invalid lifecycle", url.Values{"lifecycle": {"bogus"}}, 422, "validation_failed", "lifecycle"},
		{"invalid created_after", url.Values{"created_after": {"yesterday"}}, 422, "validation_failed", "created_after"},
		{"invalid created_before", url.Values{"created_before": {"tomorrow"}}, 422, "validation_failed", "created_before"},
		{"equal time bounds", url.Values{
			"created_after":  {"2026-05-01T10:00:00Z"},
			"created_before": {"2026-05-01T10:00:00Z"},
		}, 422, "validation_failed", "created_before"},
		{"inverted time bounds", url.Values{
			"created_after":  {"2026-05-01T11:00:00Z"},
			"created_before": {"2026-05-01T10:00:00Z"},
		}, 422, "validation_failed", "created_before"},
		{"limit zero", url.Values{"limit": {"0"}}, 422, "validation_failed", "limit"},
		{"limit too high", url.Values{"limit": {"201"}}, 422, "validation_failed", "limit"},
		{"limit non-integer", url.Values{"limit": {"abc"}}, 422, "validation_failed", "limit"},
		{"malformed cursor", url.Values{"cursor": {"bad"}}, 400, "invalid_cursor", ""},
	}
	for _, tc := range invalidCases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs?"+tc.params.Encode(), nil)
		_, err := parseListQuery(req, scopedPrincipal)
		p, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || p.Status != tc.wantStatus || p.Code != tc.wantCode || p.Field != tc.wantField {
			t.Errorf("%s: got %v, want status=%d code=%q field=%q", tc.name, err, tc.wantStatus, tc.wantCode, tc.wantField)
		}
	}

	reqDefault := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs", nil)
	gotDefault, err := parseListQuery(reqDefault, globalPrincipal)
	if err != nil || gotDefault.Limit != 50 || gotDefault.Order != media.OrderDescending {
		t.Fatalf("default parseListQuery = (%+v, %v)", gotDefault, err)
	}

	keyID := uuid.NewString()
	providerID := uuid.NewString()
	cursorTime := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	cursorID := uuid.NewString()
	params := url.Values{
		"api_key_id":     {strings.ToUpper(keyID)},
		"provider_id":    {providerID},
		"route":          {"studio"},
		"state":          {"running"},
		"lifecycle":      {"active"},
		"created_after":  {"2026-05-01T08:00:00Z"},
		"created_before": {"2026-05-01T10:00:00Z"},
		"limit":          {"25"},
		"cursor":         {encodeCursor(jobs.Record{CreatedAt: cursorTime, ID: cursorID})},
	}
	reqFull := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs?"+params.Encode(), nil)
	got, err := parseListQuery(reqFull, scopedPrincipal)
	if err != nil {
		t.Fatalf("valid parseListQuery failed: %v", err)
	}
	if got.Limit != 25 || got.APIKeyID == nil || *got.APIKeyID != keyID ||
		got.ProviderID == nil || *got.ProviderID != providerID ||
		got.State == nil || *got.State != jobs.Running ||
		got.Lifecycle == nil || *got.Lifecycle != jobs.Active ||
		got.CreatedAfter == nil || got.CreatedBefore == nil ||
		got.After == nil || got.After.ID != cursorID || !got.After.CreatedAt.Equal(cursorTime) {
		t.Fatalf("parsed query mismatch: %+v", got)
	}
}

func TestJobItemWireJSON(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	id, keyID, provID := uuid.New(), uuid.New(), uuid.New()

	if _, err := jobItem(jobs.Record{ID: "bad", APIKeyID: keyID.String(), ProviderID: provID.String()}); err == nil {
		t.Fatal("jobItem with invalid UUID succeeded")
	}

	minimal, err := jobItem(jobs.Record{
		ID: id.String(), APIKeyID: keyID.String(), ProviderID: provID.String(),
		ProviderName: "video", UpstreamModel: "sora-2", RouteSlug: "studio",
		Operation: media.OpVideoCreate, Surface: "openai", State: jobs.Queued,
		Lifecycle: jobs.Creating, ETag: uuid.NewString(), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("jobItem(minimal) = %v", err)
	}
	rawMin, err := json.Marshal(minimal)
	if err != nil {
		t.Fatal(err)
	}
	var minFields map[string]json.RawMessage
	if err := json.Unmarshal(rawMin, &minFields); err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"upstream_job_id", "progress_percent", "expires_at", "error_class", "completed_at", "last_polled_at", "reconciliation_error", "deleted_at"} {
		if _, present := minFields[omitted]; present {
			t.Errorf("minimal item unexpectedly included %q: %s", omitted, rawMin)
		}
	}

	progress := float32(100)
	full, err := jobItem(jobs.Record{
		ID: id.String(), APIKeyID: keyID.String(), ProviderID: provID.String(),
		ProviderName: "video", UpstreamModel: "sora-2", RouteSlug: "studio",
		Operation: media.OpVideoCreate, Surface: "openai", State: jobs.Succeeded,
		Lifecycle: jobs.Active, ETag: uuid.NewString(), CreatedAt: now, UpdatedAt: now,
		UpstreamJobID: new("vid_1"), ProgressPercent: &progress, ExpiresAt: &now,
		ErrorClass: new("none"), CompletedAt: &now, LastPolledAt: &now,
		ReconciliationError: new("warn"), DeletedAt: &now,
	})
	if err != nil {
		t.Fatalf("jobItem(full) = %v", err)
	}
	if !full.ContentAvailable || !full.UpstreamJobId.IsSpecified() || full.UpstreamJobId.MustGet() != "vid_1" ||
		!full.ProgressPercent.IsSpecified() || full.ProgressPercent.MustGet() != 100 {
		t.Fatalf("full item = %+v", full)
	}
}

func TestProblemMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind       jobs.Kind
		wantStatus int
		wantCode   string
	}{
		{jobs.KindNotFound, http.StatusNotFound, "not_found"},
		{jobs.KindChanged, http.StatusPreconditionFailed, "etag_mismatch"},
		{jobs.KindConflict, http.StatusConflict, "media_job_upstream_identity_conflict"},
		{jobs.KindBusy, http.StatusConflict, "media_job_busy"},
		{jobs.KindInvalid, http.StatusUnprocessableEntity, "validation_failed"},
		{jobs.KindCallerCredential, http.StatusConflict, "media_job_caller_credential"},
	}
	for _, tc := range cases {
		mapped := jobProblem(&jobs.Error{Kind: tc.kind, Message: "detail"})
		p, ok := errors.AsType[*httpkit.Problem](mapped)
		if !ok || p.Status != tc.wantStatus || p.Code != tc.wantCode {
			t.Errorf("jobProblem(%v) = %v, want status=%d code=%q", tc.kind, mapped, tc.wantStatus, tc.wantCode)
		}
	}

	unknown := errors.New("boom")
	if got := jobProblem(unknown); !errors.Is(got, unknown) {
		t.Fatalf("jobProblem(unknown) = %v, want %v", got, unknown)
	}
	dbErr := &jobs.Error{Kind: jobs.KindDatabase, Message: "db down"}
	if got := jobProblem(dbErr); !errors.Is(got, dbErr) {
		t.Fatalf("jobProblem(KindDatabase) = %v, want passthrough", got)
	}
}

type fakeRow struct {
	err    error
	values []any
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.values[i].(string)
		case **string:
			*d = r.values[i].(*string)
		case *media.Operation:
			*d = r.values[i].(media.Operation)
		case *jobs.State:
			*d = r.values[i].(jobs.State)
		case *jobs.Lifecycle:
			*d = r.values[i].(jobs.Lifecycle)
		case **float32:
			*d = r.values[i].(*float32)
		case **time.Time:
			*d = r.values[i].(*time.Time)
		case *time.Time:
			*d = r.values[i].(time.Time)
		case *int:
			*d = r.values[i].(int)
		case *bool:
			*d = r.values[i].(bool)
		default:
			return errors.New("unsupported scan type")
		}
	}
	return nil
}

type fakeRows struct {
	pgx.Rows
	items []jobs.Record
	index int
}

func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Next() bool {
	if r.index >= len(r.items) {
		return false
	}
	r.index++
	return true
}
func (r *fakeRows) Scan(dest ...any) error { return rowFor(r.items[r.index-1]).Scan(dest...) }

func rowFor(r jobs.Record) fakeRow {
	return fakeRow{values: []any{
		r.ID, r.UpstreamJobID, r.APIKeyID, r.ProjectID, r.ProviderID,
		r.ProviderName, r.UpstreamModel, r.RouteSlug, r.Operation, r.Surface, r.State, r.Lifecycle,
		r.ProgressPercent, r.ExpiresAt, r.ErrorClass, r.CompletedAt, r.LastPolledAt,
		r.ReconciliationError, r.DeletedAt, r.RuntimeGenerationID, r.ProviderRevisionID,
		r.CredentialVersionID, r.SlotID, r.ReconciliationClaimID,
		r.ReconciliationAttempts, r.NextReconciliationAt, r.LastReconciliationAt, r.ETag,
		r.CreatedAt, r.UpdatedAt, r.StrictContract, r.CallerCredential,
	}}
}

type fakeQuerier struct {
	record       *jobs.Record
	listItems    []jobs.Record
	driftOnClaim bool
	leased       bool
}

func (q *fakeQuerier) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (q *fakeQuerier) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return &fakeRows{items: q.listItems}, nil
}

func (q *fakeQuerier) Begin(_ context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "WHERE j.id = $1") {
		if q.record == nil || q.record.ID != args[0].(string) {
			return fakeRow{err: pgx.ErrNoRows}
		}
		return rowFor(*q.record)
	}
	if strings.Contains(sql, "lifecycle_state = CASE WHEN lifecycle_state = 'active' THEN 'delete_pending'") {
		if q.record == nil || q.record.ID != args[1].(string) {
			return fakeRow{err: pgx.ErrNoRows}
		}
		if q.driftOnClaim {
			q.record.ETag = uuid.NewString()
			return fakeRow{err: pgx.ErrNoRows}
		}
		if q.record.ETag != args[2].(string) || q.record.Lifecycle == jobs.Creating || q.record.Lifecycle == jobs.Deleted || q.record.CallerCredential || q.leased {
			return fakeRow{err: pgx.ErrNoRows}
		}
		updated := *q.record
		if updated.Lifecycle == jobs.Active {
			updated.Lifecycle = jobs.DeletePending
		}
		updated.ReconciliationClaimID = new(uuid.NewString())
		updated.ETag = uuid.NewString()
		q.record = &updated
		return rowFor(updated)
	}
	return fakeRow{err: pgx.ErrNoRows}
}

func sampleRecord(projectID string, state jobs.State, lifecycle jobs.Lifecycle) jobs.Record {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return jobs.Record{
		ID: uuid.NewString(), UpstreamJobID: new("vid_1"), APIKeyID: uuid.NewString(),
		ProjectID: &projectID, ProviderID: uuid.NewString(), ProviderName: "video",
		UpstreamModel: "sora-2", RouteSlug: "studio", Operation: media.OpVideoCreate,
		Surface: "openai", State: state, Lifecycle: lifecycle,
		ETag: uuid.NewString(), CreatedAt: now, UpdatedAt: now,
	}
}

func TestHandlersGetListContentAndDelete(t *testing.T) {
	t.Parallel()
	projectID := uuid.NewString()
	otherProject := uuid.NewString()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	owner := authz.Principal{AllProjects: true}
	otherViewer := authz.Principal{AllProjects: false, Projects: map[string]string{otherProject: "viewer"}}

	rec := sampleRecord(projectID, jobs.Running, jobs.Active)
	q := &fakeQuerier{
		record:    &rec,
		listItems: []jobs.Record{rec, sampleRecord(projectID, jobs.Succeeded, jobs.Active)},
	}
	api := &API{Jobs: jobs.New(jobs.Deps{Store: jobs.NewStore(q), Now: func() time.Time { return now }})}

	badGet := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs/not-a-uuid", nil)
	badGet.SetPathValue("job_id", "not-a-uuid")
	if _, err := api.get(badGet, owner); !errors.Is(err, errJobNotFound) {
		t.Fatalf("get(invalid uuid) = %v, want errJobNotFound", err)
	}

	outOfScope := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs/"+rec.ID, nil)
	outOfScope.SetPathValue("job_id", rec.ID)
	if _, err := api.get(outOfScope, otherViewer); err == nil {
		t.Fatal("get(out of project scope) succeeded")
	}

	okGet := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs/"+rec.ID, nil)
	okGet.SetPathValue("job_id", rec.ID)
	reply, err := api.get(okGet, owner)
	if err != nil || reply.Status != http.StatusOK || reply.ETag != rec.ETag {
		t.Fatalf("get(valid) = (%+v, %v)", reply, err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs?limit=1", nil)
	listReply, err := api.list(listReq, owner)
	if err != nil || listReply.Status != http.StatusOK {
		t.Fatalf("list(valid) = (%+v, %v)", listReply, err)
	}
	listBody, ok := listReply.Body.(contract.MediaJobListResponse)
	if !ok || len(listBody.Items) != 1 || !listBody.NextCursor.IsSpecified() {
		t.Fatalf("list body = %+v", listReply.Body)
	}

	badContent := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs/"+rec.ID+"/content?variant=nope", nil)
	badContent.SetPathValue("job_id", rec.ID)
	err = api.content(httptest.NewRecorder(), badContent, owner)
	if p, ok := errors.AsType[*httpkit.Problem](err); !ok || p.Status != http.StatusBadRequest {
		t.Fatalf("content(bad variant) = %v, want 400", err)
	}

	noContent := httptest.NewRequest(http.MethodGet, "/api/v1/media-jobs/"+rec.ID+"/content?variant=video", nil)
	noContent.SetPathValue("job_id", rec.ID)
	err = api.content(httptest.NewRecorder(), noContent, owner)
	if p, ok := errors.AsType[*httpkit.Problem](err); !ok || p.Status != http.StatusConflict || p.Code != "media_content_unavailable" {
		t.Fatalf("content(running job) = %v, want 409 media_content_unavailable", err)
	}

	badRefresh := httptest.NewRequest(http.MethodPost, "/api/v1/media-jobs/nope/refresh", nil)
	badRefresh.SetPathValue("job_id", "nope")
	if _, err := api.refresh(badRefresh, owner); !errors.Is(err, errJobNotFound) {
		t.Fatalf("refresh(invalid uuid) = %v, want errJobNotFound", err)
	}

	badDelete := httptest.NewRequest(http.MethodDelete, "/api/v1/media-jobs/nope", nil)
	badDelete.SetPathValue("job_id", "nope")
	if _, err := api.delete(badDelete, owner); !errors.Is(err, errJobNotFound) {
		t.Fatalf("delete(invalid uuid) = %v, want errJobNotFound", err)
	}
}

func TestClaimDeleteTxPreconditionsAndTOCTOU(t *testing.T) {
	t.Parallel()
	projectID := uuid.NewString()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	owner := authz.Principal{AllProjects: true}

	makeReq := func(id, ifMatch string) *http.Request {
		r := httptest.NewRequest(http.MethodDelete, "/api/v1/media-jobs/"+id, nil)
		r.SetPathValue("job_id", id)
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		return r
	}

	t.Run("missing If-Match returns 428", func(t *testing.T) {
		t.Parallel()
		rec := sampleRecord(projectID, jobs.Succeeded, jobs.Active)
		_, _, err := claimDeleteTx(makeReq(rec.ID, ""), jobs.NewStore(&fakeQuerier{record: &rec}), owner, now)
		p, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || p.Status != http.StatusPreconditionRequired {
			t.Fatalf("got %v, want 428", err)
		}
	})

	t.Run("mismatched If-Match returns 412", func(t *testing.T) {
		t.Parallel()
		rec := sampleRecord(projectID, jobs.Succeeded, jobs.Active)
		_, _, err := claimDeleteTx(makeReq(rec.ID, `"`+uuid.NewString()+`"`), jobs.NewStore(&fakeQuerier{record: &rec}), owner, now)
		p, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || p.Status != http.StatusPreconditionFailed || p.Code != "etag_mismatch" {
			t.Fatalf("got %v, want 412 etag_mismatch", err)
		}
	})

	t.Run("TOCTOU ETag drift during claim returns 412", func(t *testing.T) {
		t.Parallel()
		rec := sampleRecord(projectID, jobs.Succeeded, jobs.Active)
		q := &fakeQuerier{record: &rec, driftOnClaim: true}
		_, _, err := claimDeleteTx(makeReq(rec.ID, `"`+rec.ETag+`"`), jobs.NewStore(q), owner, now)
		p, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || p.Status != http.StatusPreconditionFailed || p.Code != "etag_mismatch" {
			t.Fatalf("got %v, want 412 etag_mismatch", err)
		}
	})

	t.Run("creating and leased return 409 busy", func(t *testing.T) {
		t.Parallel()
		creating := sampleRecord(projectID, jobs.Queued, jobs.Creating)
		_, _, err := claimDeleteTx(makeReq(creating.ID, `"`+creating.ETag+`"`), jobs.NewStore(&fakeQuerier{record: &creating}), owner, now)
		if p, ok := errors.AsType[*httpkit.Problem](err); !ok || p.Status != http.StatusConflict || p.Code != "media_job_busy" {
			t.Fatalf("creating got %v, want 409 media_job_busy", err)
		}

		active := sampleRecord(projectID, jobs.Succeeded, jobs.Active)
		_, _, err = claimDeleteTx(makeReq(active.ID, `"`+active.ETag+`"`), jobs.NewStore(&fakeQuerier{record: &active, leased: true}), owner, now)
		if p, ok := errors.AsType[*httpkit.Problem](err); !ok || p.Status != http.StatusConflict || p.Code != "media_job_busy" {
			t.Fatalf("leased got %v, want 409 media_job_busy", err)
		}
	})

	t.Run("caller credential returns 409", func(t *testing.T) {
		t.Parallel()
		rec := sampleRecord(projectID, jobs.Succeeded, jobs.Active)
		rec.CallerCredential = true
		_, _, err := claimDeleteTx(makeReq(rec.ID, `"`+rec.ETag+`"`), jobs.NewStore(&fakeQuerier{record: &rec}), owner, now)
		if p, ok := errors.AsType[*httpkit.Problem](err); !ok || p.Status != http.StatusConflict || p.Code != "media_job_caller_credential" {
			t.Fatalf("caller credential got %v, want 409 media_job_caller_credential", err)
		}
	})

	t.Run("already deleted audits immediately without reconciling", func(t *testing.T) {
		t.Parallel()
		rec := sampleRecord(projectID, jobs.Succeeded, jobs.Deleted)
		plan, event, err := claimDeleteTx(makeReq(rec.ID, `"`+rec.ETag+`"`), jobs.NewStore(&fakeQuerier{record: &rec}), owner, now)
		if err != nil || plan.needsReconcile {
			t.Fatalf("already deleted = (%+v, %v), want needsReconcile=false", plan, err)
		}
		if event.Action != "media_job.delete" || event.ID != rec.ID || event.Project == nil || *event.Project != projectID {
			t.Fatalf("already deleted event = %+v", event)
		}
	})

	t.Run("active job claims atomically and transitions to delete_pending", func(t *testing.T) {
		t.Parallel()
		rec := sampleRecord(projectID, jobs.Succeeded, jobs.Active)
		plan, event, err := claimDeleteTx(makeReq(rec.ID, `"`+rec.ETag+`"`), jobs.NewStore(&fakeQuerier{record: &rec}), owner, now)
		if err != nil || !plan.needsReconcile || plan.claimed.Lifecycle != jobs.DeletePending || event != (access.Event{}) {
			t.Fatalf("active claim = (%+v, %+v, %v)", plan, event, err)
		}
	})
}

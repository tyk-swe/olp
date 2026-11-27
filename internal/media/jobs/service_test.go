package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

type stubRow struct {
	err    error
	values []any
}

func (r stubRow) Scan(dest ...any) error {
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
		case *State:
			*d = r.values[i].(State)
		case *Lifecycle:
			*d = r.values[i].(Lifecycle)
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
		case *[]byte:
			*d = r.values[i].([]byte)
		default:
			return errors.New("unsupported scan type")
		}
	}
	return nil
}

func recordRow(r Record) stubRow {
	return stubRow{values: []any{
		r.ID, r.UpstreamJobID, r.APIKeyID, r.ProjectID, r.ProviderID,
		r.ProviderName, r.UpstreamModel, r.RouteSlug, r.Operation, r.Surface, r.State, r.Lifecycle,
		r.ProgressPercent, r.ExpiresAt, r.ErrorClass, r.CompletedAt, r.LastPolledAt,
		r.ReconciliationError, r.DeletedAt, r.RuntimeGenerationID, r.ProviderRevisionID,
		r.CredentialVersionID, r.SlotID, r.ReconciliationClaimID,
		r.ReconciliationAttempts, r.NextReconciliationAt, r.LastReconciliationAt, r.ETag,
		r.CreatedAt, r.UpdatedAt, r.StrictContract, r.CallerCredential,
	}}
}

type stubDB struct {
	mu            sync.Mutex
	records       map[string]Record
	claimedUntil  map[string]*time.Time
	snapshots     map[string][]byte
	attachErr     error
	finalizeErr   error
	finishErr     error
	extendRefused bool
}

func newStubDB() *stubDB {
	return &stubDB{
		records:      map[string]Record{},
		claimedUntil: map[string]*time.Time{},
		snapshots:    map[string][]byte{},
	}
}

func (db *stubDB) put(r Record) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.records[r.ID] = r
}

func (db *stubDB) get(id string) Record {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.records[id]
}

func (db *stubDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	switch {
	case strings.Contains(sql, "reconciliation_claimed_until = GREATEST"):
		id, claim, until := args[0].(string), args[1].(string), args[2].(time.Time)
		r, ok := db.records[id]
		if db.extendRefused || !ok || r.ReconciliationClaimID == nil || *r.ReconciliationClaimID != claim {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		r.NextReconciliationAt = until
		r.ETag = uuid.NewString()
		db.claimedUntil[id] = &until
		db.records[id] = r
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "reconciliation_claim_id = NULL"):
		if db.finishErr != nil {
			return pgconn.CommandTag{}, db.finishErr
		}
		id, claim, next, failure := args[0].(string), args[1].(string), args[2].(time.Time), args[3].(*string)
		r, ok := db.records[id]
		if !ok || r.ReconciliationClaimID == nil || *r.ReconciliationClaimID != claim {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		r.ReconciliationClaimID = nil
		db.claimedUntil[id] = nil
		r.NextReconciliationAt = next
		r.ReconciliationError = failure
		if failure == nil {
			r.ReconciliationAttempts = 0
		}
		r.ETag = uuid.NewString()
		db.records[id] = r
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func (db *stubDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}

func (db *stubDB) Begin(_ context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

func (db *stubDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	db.mu.Lock()
	defer db.mu.Unlock()

	switch {
	case strings.Contains(sql, "FROM olp.runtime_releases"):
		snap, ok := db.snapshots[args[0].(string)]
		if !ok {
			return stubRow{err: pgx.ErrNoRows}
		}
		return stubRow{values: []any{snap}}

	case strings.Contains(sql, "SELECT EXISTS(SELECT 1 FROM olp.media_jobs"):
		_, ok := db.records[args[0].(string)]
		return stubRow{values: []any{ok}}

	case strings.Contains(sql, "WHERE j.id = $1"):
		r, ok := db.records[args[0].(string)]
		if !ok {
			return stubRow{err: pgx.ErrNoRows}
		}
		return recordRow(r)

	case strings.Contains(sql, "lifecycle_state = CASE WHEN lifecycle_state = 'active' THEN 'delete_pending'"):
		now, id, etag := args[0].(time.Time), args[1].(string), args[2].(string)
		r, ok := db.records[id]
		if !ok || r.ETag != etag || r.Lifecycle == Creating || r.Lifecycle == Deleted || r.CallerCredential {
			return stubRow{err: pgx.ErrNoRows}
		}
		if until := db.claimedUntil[id]; until != nil && until.After(now) {
			return stubRow{err: pgx.ErrNoRows}
		}
		if r.Lifecycle == Active {
			r.Lifecycle = DeletePending
			r.ReconciliationError = nil
		}
		return db.applyClaim(r, now)

	case strings.Contains(sql, "WHERE id = $2 AND lifecycle_state <> 'deleted'"):
		now, id := args[0].(time.Time), args[1].(string)
		r, ok := db.records[id]
		if !ok || r.Lifecycle == Deleted {
			return stubRow{err: pgx.ErrNoRows}
		}
		if until := db.claimedUntil[id]; until != nil && until.After(now) {
			return stubRow{err: pgx.ErrNoRows}
		}
		return db.applyClaim(r, now)

	case strings.Contains(sql, "lifecycle_state = 'active'") && strings.Contains(sql, "WHERE id = $1 AND lifecycle_state = 'creating'"):
		if db.attachErr != nil {
			return stubRow{err: db.attachErr}
		}
		id, upstreamID := args[0].(string), args[1].(string)
		r, ok := db.records[id]
		if !ok || r.Lifecycle != Creating {
			return stubRow{err: pgx.ErrNoRows}
		}
		r.UpstreamJobID = &upstreamID
		r.State = State(args[2].(string))
		r.Lifecycle = Active
		r.ProgressPercent = args[3].(*float32)
		r.ExpiresAt = args[4].(*time.Time)
		r.ErrorClass = args[5].(*string)
		polled := args[6].(time.Time)
		r.LastPolledAt = &polled
		r.ReconciliationError = nil
		r.ETag = uuid.NewString()
		db.records[id] = r
		return recordRow(r)

	case strings.Contains(sql, "SET lifecycle_state = $2"):
		id, lifecycle := args[0].(string), Lifecycle(args[1].(string))
		upstreamPtr, reason := args[2].(*string), args[3].(string)
		allowed, claim := args[4].([]Lifecycle), args[5].(*string)
		r, ok := db.records[id]
		if !ok || !containsLifecycle(allowed, r.Lifecycle) || !fenceMatches(r, claim) {
			return stubRow{err: pgx.ErrNoRows}
		}
		r.Lifecycle = lifecycle
		if upstreamPtr != nil {
			r.UpstreamJobID = upstreamPtr
		}
		r.ReconciliationError = &reason
		r.ETag = uuid.NewString()
		db.records[id] = r
		return recordRow(r)

	case strings.Contains(sql, "SET lifecycle_state = 'delete_pending'"):
		id, claim := args[0].(string), args[1].(*string)
		r, ok := db.records[id]
		if !ok || r.Lifecycle != Active || !fenceMatches(r, claim) {
			return stubRow{err: pgx.ErrNoRows}
		}
		r.Lifecycle = DeletePending
		r.ReconciliationError = nil
		r.ETag = uuid.NewString()
		db.records[id] = r
		return recordRow(r)

	case strings.Contains(sql, "SET lifecycle_state = 'deleted'"):
		if db.finalizeErr != nil {
			return stubRow{err: db.finalizeErr}
		}
		id, claim := args[0].(string), args[1].(*string)
		r, ok := db.records[id]
		allowed := []Lifecycle{Creating, CreateAmbiguous, CleanupPending, DeletePending}
		if !ok || !containsLifecycle(allowed, r.Lifecycle) || !fenceMatches(r, claim) {
			return stubRow{err: pgx.ErrNoRows}
		}
		r.Lifecycle = Deleted
		if r.DeletedAt == nil {
			r.DeletedAt = new(time.Now().UTC())
		}
		r.ReconciliationError = nil
		r.ETag = uuid.NewString()
		db.records[id] = r
		return recordRow(r)

	case strings.Contains(sql, "progress_percent = CASE"):
		id, nextState := args[0].(string), State(args[1].(string))
		progress, expires := args[2].(*float32), args[3].(*time.Time)
		failure, polled, claim := args[4].(*string), args[5].(time.Time), args[7].(*string)
		r, ok := db.records[id]
		if !ok || r.Lifecycle != Active || !fenceMatches(r, claim) {
			return stubRow{err: pgx.ErrNoRows}
		}
		r.State = nextState
		if progress != nil {
			r.ProgressPercent = progress
		}
		if expires != nil {
			r.ExpiresAt = expires
		}
		if failure != nil {
			r.ErrorClass = failure
		}
		r.LastPolledAt = &polled
		if !nextState.Polling() && r.CompletedAt == nil {
			r.CompletedAt = &polled
		}
		r.ETag = uuid.NewString()
		db.records[id] = r
		return recordRow(r)
	}
	return stubRow{err: errors.New("unexpected QueryRow: " + sql)}
}

func (db *stubDB) applyClaim(r Record, now time.Time) stubRow {
	claimID := uuid.NewString()
	until := now.Add(2 * time.Minute)
	r.ReconciliationClaimID = &claimID
	r.LastReconciliationAt = &now
	r.NextReconciliationAt = until
	r.ReconciliationAttempts++
	r.ETag = uuid.NewString()
	db.claimedUntil[r.ID] = &until
	db.records[r.ID] = r
	return recordRow(r)
}

func containsLifecycle(allowed []Lifecycle, got Lifecycle) bool {
	for _, item := range allowed {
		if item == got {
			return true
		}
	}
	return false
}

func fenceMatches(r Record, claim *string) bool {
	if claim == nil {
		return true
	}
	return r.ReconciliationClaimID != nil && *r.ReconciliationClaimID == *claim
}

type stubCredentials struct {
	eligibility routing.Eligibility
	secret      []byte
	secretErr   error
}

func (c stubCredentials) Eligibility(string) routing.Eligibility {
	if c.eligibility == "" {
		return routing.Eligible
	}
	return c.eligibility
}

func (c stubCredentials) Secret(context.Context, *release.Release, string) ([]byte, int64, error) {
	if c.secretErr != nil {
		return nil, 0, c.secretErr
	}
	return c.secret, 1, nil
}

func (c stubCredentials) NetworkSecret(context.Context, *release.Release, string, string) ([]byte, error) {
	return nil, nil
}

type testEnv struct {
	db         *stubDB
	store      *Store
	service    *Service
	spool      *staging.Spool
	gaps       *atomic.Uint64
	now        time.Time
	genID      string
	providerID string
	revID      string
	credID     string
	slotID     string
	baseURL    string
}

func newTestEnv(t *testing.T, handler http.HandlerFunc) *testEnv {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	spool, err := staging.NewSpool(t.TempDir(), 256<<20, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })

	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
		PlainHTTPHosts:  []string{"127.0.0.1", "localhost"},
	}
	mt := transport.New(transport.Deps{
		Client:           srv.Client(),
		Auth:             connectors.NewAuth(policy, func(string) string { return "" }),
		Egress:           policy,
		Spool:            spool,
		MaxResponseBytes: 1 << 20,
		Log:              slog.New(slog.DiscardHandler),
	})

	db := newStubDB()
	env := &testEnv{
		db:         db,
		store:      NewStore(db),
		spool:      spool,
		gaps:       &atomic.Uint64{},
		now:        time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		genID:      uuid.NewString(),
		providerID: uuid.NewString(),
		revID:      uuid.NewString(),
		credID:     uuid.NewString(),
		slotID:     uuid.NewString(),
		baseURL:    srv.URL,
	}

	snap, err := json.Marshal(map[string]any{
		"providers": map[string]any{env.providerID: map[string]any{
			"id":                env.providerID,
			"revision_id":       env.revID,
			"name":              "video",
			"kind":              "openai_compatible",
			"active_credential": env.credID,
			"endpoint":          srv.URL,
			"slots":             []map[string]any{{"id": env.slotID, "credential_id": env.credID}},
		}},
		"routes": map[string]any{"studio": map[string]any{
			"slug":            "studio",
			"revision_id":     uuid.NewString(),
			"overall_timeout": 5000,
			"targets":         []map[string]any{{"provider_id": env.providerID, "provider_model": "sora-2"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	db.snapshots[env.genID] = snap
	env.service = New(Deps{
		Store:       env.store,
		Transport:   mt,
		Credentials: stubCredentials{secret: []byte("sk-test")},
		Gaps:        env.gaps,
		Now:         func() time.Time { return env.now },
	})
	return env
}

func (e *testEnv) record(state State, lifecycle Lifecycle, upstreamID string) Record {
	var upstream *string
	if upstreamID != "" {
		upstream = &upstreamID
	}
	return Record{
		ID:                  uuid.NewString(),
		UpstreamJobID:       upstream,
		APIKeyID:            uuid.NewString(),
		ProjectID:           new(uuid.NewString()),
		ProviderID:          e.providerID,
		ProviderName:        "video",
		UpstreamModel:       "sora-2",
		RouteSlug:           "studio",
		Operation:           media.OpVideoCreate,
		Surface:             "openai",
		State:               state,
		Lifecycle:           lifecycle,
		RuntimeGenerationID: e.genID,
		ProviderRevisionID:  e.revID,
		CredentialVersionID: &e.credID,
		SlotID:              e.slotID,
		ETag:                uuid.NewString(),
		CreatedAt:           e.now.Add(-time.Minute),
		UpdatedAt:           e.now.Add(-time.Minute),
	}
}

func TestNewDefaultsAndNilGuards(t *testing.T) {
	t.Parallel()
	db := newStubDB()
	svc := New(Deps{Store: NewStore(db)})

	if svc.Store() == nil {
		t.Fatal("Store() returned nil")
	}
	if svc.Now().IsZero() {
		t.Fatal("default Now() returned zero time")
	}
	svc.RecordGap()

	genID := uuid.NewString()
	providerID := uuid.NewString()
	revID := uuid.NewString()
	credID := uuid.NewString()
	slotID := uuid.NewString()
	snap, _ := json.Marshal(map[string]any{
		"providers": map[string]any{providerID: map[string]any{
			"id": providerID, "revision_id": revID, "name": "video", "kind": "openai_compatible",
			"active_credential": credID,
			"endpoint":          "https://example.test",
			"slots":             []map[string]any{{"id": slotID, "credential_id": credID}},
		}},
		"routes": map[string]any{"studio": map[string]any{
			"slug": "studio", "revision_id": uuid.NewString(), "overall_timeout": 5000,
			"targets": []map[string]any{{"provider_id": providerID, "provider_model": "sora-2"}},
		}},
	})
	db.snapshots[genID] = snap
	_, err := svc.Target(t.Context(), Record{
		RuntimeGenerationID: genID, ProviderID: providerID, ProviderRevisionID: revID,
		RouteSlug: "studio", UpstreamModel: "sora-2", SlotID: slotID, CredentialVersionID: &credID,
	})
	var unavailable *Unavailable
	if !errors.As(err, &unavailable) || unavailable.Code != "media_job_runtime_unavailable" {
		t.Fatalf("Target with nil credentials = %v, want media_job_runtime_unavailable", err)
	}
}

func TestStoreClaimForDeleteAndReleaseSnapshot(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ctx := t.Context()

	if env.store.Health() == nil {
		t.Fatal("Health() returned nil")
	}
	if raw, err := env.store.ReleaseSnapshot(ctx, env.genID); err != nil || len(raw) == 0 {
		t.Fatalf("ReleaseSnapshot(existing) = (%d bytes, %v)", len(raw), err)
	}
	if _, err := env.store.ReleaseSnapshot(ctx, uuid.NewString()); err == nil {
		t.Fatal("ReleaseSnapshot(missing) succeeded, want error")
	} else if kind, _ := KindOf(err); kind != KindNotFound {
		t.Fatalf("ReleaseSnapshot(missing) kind = %v, want KindNotFound", kind)
	}

	active := env.record(Running, Active, "vid_active")
	env.db.put(active)
	if _, _, err := env.store.ClaimForDelete(ctx, active.ID, "not-a-uuid", env.now); err == nil {
		t.Fatal("ClaimForDelete(non-uuid etag) succeeded")
	} else if kind, _ := KindOf(err); kind != KindChanged {
		t.Fatalf("ClaimForDelete(non-uuid etag) kind = %v, want KindChanged", kind)
	}
	if _, _, err := env.store.ClaimForDelete(ctx, active.ID, uuid.NewString(), env.now); err == nil {
		t.Fatal("ClaimForDelete(drifted etag) succeeded")
	} else if kind, _ := KindOf(err); kind != KindChanged {
		t.Fatalf("ClaimForDelete(drifted etag) kind = %v, want KindChanged", kind)
	}

	claimed, needsReconcile, err := env.store.ClaimForDelete(ctx, active.ID, active.ETag, env.now)
	if err != nil || !needsReconcile || claimed.Lifecycle != DeletePending || claimed.ETag == active.ETag {
		t.Fatalf("ClaimForDelete(active) = (%+v, %v, %v)", claimed, needsReconcile, err)
	}
	if _, _, err := env.store.ClaimForDelete(ctx, active.ID, claimed.ETag, env.now); err == nil {
		t.Fatal("ClaimForDelete(leased) succeeded")
	} else if kind, _ := KindOf(err); kind != KindBusy {
		t.Fatalf("ClaimForDelete(leased) kind = %v, want KindBusy", kind)
	}

	deleted := env.record(Succeeded, Deleted, "vid_done")
	env.db.put(deleted)
	got, needsReconcile, err := env.store.ClaimForDelete(ctx, deleted.ID, deleted.ETag, env.now)
	if err != nil || needsReconcile || got.Lifecycle != Deleted {
		t.Fatalf("ClaimForDelete(deleted) = (%+v, %v, %v)", got, needsReconcile, err)
	}

	creating := env.record(Queued, Creating, "")
	env.db.put(creating)
	if _, _, err := env.store.ClaimForDelete(ctx, creating.ID, creating.ETag, env.now); err == nil {
		t.Fatal("ClaimForDelete(creating) succeeded")
	} else if kind, _ := KindOf(err); kind != KindBusy {
		t.Fatalf("ClaimForDelete(creating) kind = %v, want KindBusy", kind)
	}

	caller := env.record(Running, Active, "vid_caller")
	caller.CallerCredential = true
	env.db.put(caller)
	if _, _, err := env.store.ClaimForDelete(ctx, caller.ID, caller.ETag, env.now); err == nil {
		t.Fatal("ClaimForDelete(caller) succeeded")
	} else if kind, _ := KindOf(err); kind != KindCallerCredential {
		t.Fatalf("ClaimForDelete(caller) kind = %v, want KindCallerCredential", kind)
	}

	if _, _, err := env.store.ClaimForDelete(ctx, uuid.NewString(), uuid.NewString(), env.now); err == nil {
		t.Fatal("ClaimForDelete(missing) succeeded")
	} else if kind, _ := KindOf(err); kind != KindNotFound {
		t.Fatalf("ClaimForDelete(missing) kind = %v, want KindNotFound", kind)
	}
}

func TestServiceAttachAbandonFinalize(t *testing.T) {
	t.Parallel()
	var deletedPaths []string
	var mu sync.Mutex
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deletedPaths = append(deletedPaths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"vid_comp","object":"video.deleted","deleted":true}`)
	})
	ctx := t.Context()

	rNil := env.record(Queued, Creating, "")
	env.db.put(rNil)
	var rejected *Rejected
	if err := env.service.Attach(ctx, rNil, nil, nil); !errors.As(err, &rejected) {
		t.Fatalf("Attach(nil) = %v, want *Rejected", err)
	}
	if env.db.get(rNil.ID).Lifecycle != CreateAmbiguous {
		t.Fatalf("Attach(nil) lifecycle = %q, want create_ambiguous", env.db.get(rNil.ID).Lifecycle)
	}

	rBadID := env.record(Queued, Creating, "")
	env.db.put(rBadID)
	if err := env.service.Attach(ctx, rBadID, &media.VideoJobResult{ID: " bad ", Status: "queued"}, nil); !errors.As(err, &rejected) {
		t.Fatalf("Attach(bad id) = %v, want *Rejected", err)
	}
	if env.db.get(rBadID.ID).Lifecycle != CreateAmbiguous {
		t.Fatalf("Attach(bad id) lifecycle = %q, want create_ambiguous", env.db.get(rBadID.ID).Lifecycle)
	}

	rBadStatus := env.record(Queued, Creating, "")
	env.db.put(rBadStatus)
	if err := env.service.Attach(ctx, rBadStatus, &media.VideoJobResult{ID: "vid_bad_status", Status: "weird"}, nil); !errors.As(err, &rejected) {
		t.Fatalf("Attach(bad status) = %v, want *Rejected", err)
	}
	if got := env.db.get(rBadStatus.ID); got.Lifecycle != CleanupPending || got.UpstreamJobID == nil || *got.UpstreamJobID != "vid_bad_status" {
		t.Fatalf("Attach(bad status) record = %+v", got)
	}

	rOK := env.record(Queued, Creating, "")
	env.db.put(rOK)
	if err := env.service.Attach(ctx, rOK, &media.VideoJobResult{ID: "vid_ok", Status: "queued"}, nil); err != nil {
		t.Fatalf("Attach(valid) = %v", err)
	}
	if got := env.db.get(rOK.ID); got.Lifecycle != Active || *got.UpstreamJobID != "vid_ok" {
		t.Fatalf("Attach(valid) record = %+v", got)
	}

	rComp := env.record(Queued, Creating, "")
	env.db.put(rComp)
	env.db.attachErr = errors.New("temporary db outage")
	if err := env.service.Attach(ctx, rComp, &media.VideoJobResult{ID: "vid_comp", Status: "queued"}, nil); !errors.Is(err, ErrReconciling) {
		t.Fatalf("Attach(compensated) = %v, want ErrReconciling", err)
	}
	if env.db.get(rComp.ID).Lifecycle != Deleted || env.gaps.Load() != 0 {
		t.Fatalf("compensated record = %+v, gaps=%d", env.db.get(rComp.ID), env.gaps.Load())
	}

	rConflict := env.record(Queued, Creating, "")
	env.db.put(rConflict)
	env.db.attachErr = &pgconn.PgError{ConstraintName: "media_jobs_upstream_unique_idx"}
	if err := env.service.Attach(ctx, rConflict, &media.VideoJobResult{ID: "vid_conflict", Status: "queued"}, nil); !errors.Is(err, ErrReconciling) {
		t.Fatalf("Attach(conflict) = %v, want ErrReconciling", err)
	}
	if env.gaps.Load() != 1 {
		t.Fatalf("conflict gaps = %d, want 1", env.gaps.Load())
	}
	env.db.attachErr = nil

	rAbandonAmbig := env.record(Queued, Creating, "")
	env.db.put(rAbandonAmbig)
	env.service.Abandon(ctx, rAbandonAmbig.ID, true)
	if env.db.get(rAbandonAmbig.ID).Lifecycle != CreateAmbiguous {
		t.Fatalf("Abandon(ambiguous) lifecycle = %q", env.db.get(rAbandonAmbig.ID).Lifecycle)
	}

	rAbandonClean := env.record(Queued, Creating, "")
	env.db.put(rAbandonClean)
	env.service.Abandon(ctx, rAbandonClean.ID, false)
	if env.db.get(rAbandonClean.ID).Lifecycle != Deleted {
		t.Fatalf("Abandon(clean) lifecycle = %q", env.db.get(rAbandonClean.ID).Lifecycle)
	}

	env.db.finalizeErr = errors.New("db error")
	if env.service.Finalize(ctx, rAbandonAmbig.ID) {
		t.Fatal("Finalize on db error returned true")
	}
	if env.gaps.Load() != 2 {
		t.Fatalf("finalize failure gaps = %d, want 2", env.gaps.Load())
	}
}

func TestServiceReconcileStateMachine(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/videos/vid_poll":
			_, _ = io.WriteString(w, `{"id":"vid_poll","object":"video","status":"completed","model":"sora-2","progress":100}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/videos/vid_gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"gone"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/videos/vid_declined":
			_, _ = io.WriteString(w, `{"id":"vid_declined","object":"video.deleted","deleted":false}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/videos/vid_500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/videos/")
			_, _ = io.WriteString(w, `{"id":"`+id+`","object":"video.deleted","deleted":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := t.Context()

	polling := env.record(Running, Active, "vid_poll")
	env.db.put(polling)
	refreshed, err := env.service.Refresh(ctx, polling.ID)
	if err != nil || refreshed.State != Succeeded || refreshed.ProgressPercent == nil || *refreshed.ProgressPercent != 100 || refreshed.ReconciliationClaimID != nil {
		t.Fatalf("Refresh(active) = (%+v, %v)", refreshed, err)
	}

	retired := env.record(Succeeded, Active, "vid_expired")
	retired.ExpiresAt = new(env.now.Add(-time.Minute))
	env.db.put(retired)
	claimedRetired, ok, err := env.store.ClaimOne(ctx, retired.ID, env.now)
	if err != nil || !ok {
		t.Fatalf("ClaimOne(retired) = (%v, %v)", ok, err)
	}
	settledRetired, err := env.service.ReconcileClaimed(ctx, claimedRetired)
	if err != nil || settledRetired.Lifecycle != Deleted {
		t.Fatalf("ReconcileClaimed(retired) = (%+v, %v)", settledRetired, err)
	}

	creatingNoID := env.record(Queued, Creating, "")
	creatingNoID.NextReconciliationAt = env.now.Add(-time.Second)
	env.db.put(creatingNoID)
	afterCreating, err := env.service.Refresh(ctx, creatingNoID.ID)
	if err != nil || afterCreating.Lifecycle != CreateAmbiguous || afterCreating.ReconciliationError == nil || *afterCreating.ReconciliationError != "upstream_create_outcome_unknown" {
		t.Fatalf("Refresh(stale creating) = (%+v, %v)", afterCreating, err)
	}

	creatingWithID := env.record(Queued, Creating, "vid_stale_create")
	creatingWithID.NextReconciliationAt = env.now.Add(-time.Second)
	env.db.put(creatingWithID)
	cleaned, err := env.service.Refresh(ctx, creatingWithID.ID)
	if err != nil || cleaned.Lifecycle != Deleted {
		t.Fatalf("Refresh(stale creating with upstream) = (%+v, %v)", cleaned, err)
	}

	ambigWithID := env.record(Queued, CreateAmbiguous, "vid_gone")
	env.db.put(ambigWithID)
	cleanedAmbig, err := env.service.Refresh(ctx, ambigWithID.ID)
	if err != nil || cleanedAmbig.Lifecycle != Deleted {
		t.Fatalf("Refresh(ambiguous with 404 delete) = (%+v, %v)", cleanedAmbig, err)
	}

	declined := env.record(Succeeded, Active, "vid_declined")
	env.db.put(declined)
	afterDeclined, err := env.service.Delete(ctx, declined.ID)
	if err != nil || afterDeclined.Lifecycle != DeletePending || afterDeclined.ReconciliationError == nil || *afterDeclined.ReconciliationError != "video_delete_not_confirmed" {
		t.Fatalf("Delete(declined) = (%+v, %v)", afterDeclined, err)
	}

	serverErr := env.record(Succeeded, Active, "vid_500")
	env.db.put(serverErr)
	after500, err := env.service.Delete(ctx, serverErr.ID)
	if err != nil || after500.Lifecycle != DeletePending || after500.ReconciliationError == nil || *after500.ReconciliationError != string(upstream.ServerError) {
		t.Fatalf("Delete(500) = (%+v, %v)", after500, err)
	}

	alreadyDeleted := env.record(Succeeded, Deleted, "vid_deleted")
	env.db.put(alreadyDeleted)
	gotDeleted, err := env.service.Delete(ctx, alreadyDeleted.ID)
	if err != nil || gotDeleted.Lifecycle != Deleted {
		t.Fatalf("Delete(already deleted) = (%+v, %v)", gotDeleted, err)
	}
}

func TestServiceContent(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/videos/vid_media/content" || r.URL.Query().Get("variant") != "thumbnail" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	})
	ctx := t.Context()

	running := env.record(Running, Active, "vid_media")
	if _, err := env.service.Content(ctx, running, "video"); !errors.Is(err, ErrNoContent) {
		t.Fatalf("Content(running) = %v, want ErrNoContent", err)
	}
	deleted := env.record(Succeeded, Deleted, "vid_media")
	if _, err := env.service.Content(ctx, deleted, "video"); !errors.Is(err, ErrNoContent) {
		t.Fatalf("Content(deleted) = %v, want ErrNoContent", err)
	}

	ready := env.record(Succeeded, Active, "vid_media")
	artifact, err := env.service.Content(ctx, ready, "thumbnail")
	if err != nil {
		t.Fatalf("Content(ready) = %v", err)
	}
	defer env.spool.Remove(artifact.Handle)
	opened, err := env.spool.Open(artifact.Handle)
	if err != nil {
		t.Fatalf("Open artifact = %v", err)
	}
	defer opened.File.Close()
	data, err := io.ReadAll(opened.File)
	if err != nil || string(data) != "PNGDATA" || artifact.ContentType != "image/png" {
		t.Fatalf("artifact = (%q, %q, %v), want (PNGDATA, image/png, nil)", string(data), artifact.ContentType, err)
	}
}

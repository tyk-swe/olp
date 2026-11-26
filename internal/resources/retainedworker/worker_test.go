package retainedworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/platform/workerhealth"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

type fakeSettlementStore struct {
	claimFn             func(ctx context.Context, limit int) ([]resources.RetainedClaim, error)
	resourceFn          func(ctx context.Context, c resources.RetainedClaim) (*resources.Resource, error)
	heartbeatFn         func(ctx context.Context, c resources.RetainedClaim) error
	finishClaimFn       func(ctx context.Context, c resources.RetainedClaim, errorClass string) error
	commitPollFn        func(ctx context.Context, c resources.RetainedClaim, r *resources.Resource, obs resources.RetainedObservation, adopt bool) error
	closeUndispatchedFn func(ctx context.Context, c resources.RetainedClaim) (bool, error)
	hasUnresolvedFn     func(ctx context.Context, id uuid.UUID) (bool, error)
	checkpointFn        func(ctx context.Context, outcome workerhealth.Outcome) error
}

func (f *fakeSettlementStore) Claim(ctx context.Context, limit int) ([]resources.RetainedClaim, error) {
	if f.claimFn != nil {
		return f.claimFn(ctx, limit)
	}
	return nil, nil
}

func (f *fakeSettlementStore) Resource(ctx context.Context, c resources.RetainedClaim) (*resources.Resource, error) {
	if f.resourceFn != nil {
		return f.resourceFn(ctx, c)
	}
	return nil, errors.New("unexpected Resource call")
}

func (f *fakeSettlementStore) Heartbeat(ctx context.Context, c resources.RetainedClaim) error {
	if f.heartbeatFn != nil {
		return f.heartbeatFn(ctx, c)
	}
	return nil
}

func (f *fakeSettlementStore) FinishClaim(ctx context.Context, c resources.RetainedClaim, errorClass string) error {
	if f.finishClaimFn != nil {
		return f.finishClaimFn(ctx, c, errorClass)
	}
	return nil
}

func (f *fakeSettlementStore) CommitPoll(ctx context.Context, c resources.RetainedClaim, r *resources.Resource, obs resources.RetainedObservation, adopt bool) error {
	if f.commitPollFn != nil {
		return f.commitPollFn(ctx, c, r, obs, adopt)
	}
	return nil
}

func (f *fakeSettlementStore) CloseUndispatched(ctx context.Context, c resources.RetainedClaim) (bool, error) {
	if f.closeUndispatchedFn != nil {
		return f.closeUndispatchedFn(ctx, c)
	}
	return false, nil
}

func (f *fakeSettlementStore) HasUnresolvedMutations(ctx context.Context, id uuid.UUID) (bool, error) {
	if f.hasUnresolvedFn != nil {
		return f.hasUnresolvedFn(ctx, id)
	}
	return false, nil
}

func (f *fakeSettlementStore) Checkpoint(ctx context.Context, outcome workerhealth.Outcome) error {
	if f.checkpointFn != nil {
		return f.checkpointFn(ctx, outcome)
	}
	return nil
}

type fakeResourceStore struct {
	getByUUIDFn func(ctx context.Context, id uuid.UUID, owner string) (*resources.Resource, error)
	discoverFn  func(ctx context.Context, parent *resources.Resource, source oif.Document) error
}

func (f *fakeResourceStore) GetByUUID(ctx context.Context, id uuid.UUID, owner string) (*resources.Resource, error) {
	if f.getByUUIDFn != nil {
		return f.getByUUIDFn(ctx, id, owner)
	}
	return nil, errors.New("unexpected GetByUUID call")
}

func (f *fakeResourceStore) DiscoverRetainedModels(ctx context.Context, parent *resources.Resource, source oif.Document) error {
	if f.discoverFn != nil {
		return f.discoverFn(ctx, parent, source)
	}
	return nil
}

type trackingBody struct {
	reader    *bytes.Reader
	bytesRead int
	closed    bool
}

func newTrackingBody(data []byte) *trackingBody {
	return &trackingBody{reader: bytes.NewReader(data)}
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.bytesRead += n
	return n, err
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

func TestPathBuilderAllKindsAndErrors(t *testing.T) {
	t.Parallel()

	parentID := uuid.MustParse("01960000-0000-7000-8000-000000000001")
	cases := []struct {
		name           string
		kind           string
		upstream       string
		parentID       *uuid.UUID
		parentUpstream string
		wantPath       string
		wantOperation  string
		wantSurface    string
		wantIdentity   string
	}{
		{
			name:          "fine_tuning_job with escaping",
			kind:          "fine_tuning_job",
			upstream:      "ftjob/123?a=b#c",
			wantPath:      "fine_tuning/jobs/ftjob%2F123%3Fa=b%23c",
			wantOperation: "fine_tuning",
			wantSurface:   "openai",
			wantIdentity:  "id",
		},
		{
			name:          "vertex_tuning_job full resource name",
			kind:          "vertex_tuning_job",
			upstream:      "projects/p1/locations/us-central1/tuningJobs/job 42",
			wantPath:      "tuningJobs/job%2042",
			wantOperation: "fine_tuning",
			wantSurface:   "native",
			wantIdentity:  "name",
		},
		{
			name:          "vertex_tuning_job plain id",
			kind:          "vertex_tuning_job",
			upstream:      "job/42",
			wantPath:      "tuningJobs/job%2F42",
			wantOperation: "fine_tuning",
			wantSurface:   "native",
			wantIdentity:  "name",
		},
		{
			name:          "vector_store",
			kind:          "vector_store",
			upstream:      "vs/1 2",
			wantPath:      "vector_stores/vs%2F1%202",
			wantOperation: "vector_store",
			wantSurface:   "openai",
			wantIdentity:  "id",
		},
		{
			name:          "container",
			kind:          "container",
			upstream:      "cntr/1 2",
			wantPath:      "containers/cntr%2F1%202",
			wantOperation: "container",
			wantSurface:   "openai",
			wantIdentity:  "id",
		},
		{
			name:          "context_cache prefixed",
			kind:          "context_cache",
			upstream:      "cachedContents/cc/1 2",
			wantPath:      "cachedContents/cc%2F1%202",
			wantOperation: "context_cache",
			wantSurface:   "gemini",
			wantIdentity:  "name",
		},
		{
			name:          "context_cache unprefixed",
			kind:          "context_cache",
			upstream:      "cc/1",
			wantPath:      "cachedContents/cc%2F1",
			wantOperation: "context_cache",
			wantSurface:   "gemini",
			wantIdentity:  "name",
		},
		{
			name:           "eval_run",
			kind:           "eval_run",
			upstream:       "run/2",
			parentID:       &parentID,
			parentUpstream: "eval/1",
			wantPath:       "evals/eval%2F1/runs/run%2F2",
			wantOperation:  "eval",
			wantSurface:    "openai",
			wantIdentity:   "id",
		},
		{
			name:           "vector_store_file",
			kind:           "vector_store_file",
			upstream:       "file/2",
			parentID:       &parentID,
			parentUpstream: "vs/1",
			wantPath:       "vector_stores/vs%2F1/files/file%2F2",
			wantOperation:  "vector_store",
			wantSurface:    "openai",
			wantIdentity:   "id",
		},
		{
			name:           "vector_store_batch",
			kind:           "vector_store_batch",
			upstream:       "batch/2",
			parentID:       &parentID,
			parentUpstream: "vs/1",
			wantPath:       "vector_stores/vs%2F1/file_batches/batch%2F2",
			wantOperation:  "vector_store",
			wantSurface:    "openai",
			wantIdentity:   "id",
		},
		{
			name:           "container_file",
			kind:           "container_file",
			upstream:       "cfile/2",
			parentID:       &parentID,
			parentUpstream: "cntr/1",
			wantPath:       "containers/cntr%2F1/files/cfile%2F2",
			wantOperation:  "container",
			wantSurface:    "openai",
			wantIdentity:   "id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotParentID uuid.UUID
			var gotOwner string
			worker := &Worker{
				resources: &fakeResourceStore{
					getByUUIDFn: func(_ context.Context, id uuid.UUID, owner string) (*resources.Resource, error) {
						gotParentID = id
						gotOwner = owner
						return &resources.Resource{UpstreamID: tc.parentUpstream}, nil
					},
				},
			}
			r := &resources.Resource{Kind: tc.kind, APIKeyID: "key-1", ParentID: tc.parentID}
			gotPath, endpoint, err := worker.path(t.Context(), r, tc.upstream)
			if err != nil {
				t.Fatalf("path() error = %v", err)
			}
			if gotPath != tc.wantPath {
				t.Fatalf("path() = %q, want %q", gotPath, tc.wantPath)
			}
			if endpoint.Kind != tc.kind || endpoint.Operation != tc.wantOperation || endpoint.Surface != tc.wantSurface || endpoint.Identity != tc.wantIdentity || endpoint.Action != "get" || endpoint.Method != http.MethodGet {
				t.Fatalf("unexpected endpoint metadata: %+v", endpoint)
			}
			if tc.parentID != nil && (gotParentID != *tc.parentID || gotOwner != "key-1") {
				t.Fatalf("GetByUUID called with (%s, %q), want (%s, %q)", gotParentID, gotOwner, *tc.parentID, "key-1")
			}
		})
	}

	errorCases := []struct {
		name      string
		kind      string
		parentID  *uuid.UUID
		parentErr error
	}{
		{name: "unknown kind", kind: "nonexistent_kind"},
		{name: "unqualified retained family", kind: "conversation"},
		{name: "eval_run missing parent", kind: "eval_run"},
		{name: "vector_store_file missing parent", kind: "vector_store_file"},
		{name: "vector_store_batch missing parent", kind: "vector_store_batch"},
		{name: "container_file missing parent", kind: "container_file"},
		{name: "parent lookup error", kind: "eval_run", parentID: &parentID, parentErr: resources.ErrNotFound},
	}

	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			worker := &Worker{
				resources: &fakeResourceStore{
					getByUUIDFn: func(context.Context, uuid.UUID, string) (*resources.Resource, error) {
						return nil, tc.parentErr
					},
				},
			}
			r := &resources.Resource{Kind: tc.kind, APIKeyID: "key-1", ParentID: tc.parentID}
			if _, _, err := worker.path(t.Context(), r, "up-1"); err == nil {
				t.Fatalf("path(%s) succeeded, want error", tc.kind)
			}
		})
	}
}

func TestObservePollResponseAdoption(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	createdAt := now.Add(-10 * time.Minute)
	resourceUUID := uuid.MustParse("01960000-0000-7000-8000-0000000000aa")

	t.Run("adopts via metadata without mutating input resource", func(t *testing.T) {
		t.Parallel()
		origMeta := json.RawMessage(fmt.Sprintf(`{"upstream_model":"gpt-4.1","created_at":%q}`, createdAt.Format(time.RFC3339Nano)))
		orig := &resources.Resource{
			UUID:       resourceUUID,
			Kind:       "fine_tuning_job",
			UpstreamID: "pending:sub-1",
			State:      "submission_unknown",
			Metadata:   slices.Clone(origMeta),
			CreatedAt:  createdAt,
		}
		var discoveredUpstream string
		var committed *resources.Resource
		var committedAdopt bool
		var committedObs resources.RetainedObservation
		settlements := &fakeSettlementStore{
			commitPollFn: func(_ context.Context, _ resources.RetainedClaim, r *resources.Resource, obs resources.RetainedObservation, adopt bool) error {
				copied := copyResource(r)
				committed = &copied
				committedAdopt = adopt
				committedObs = obs
				return nil
			},
		}
		worker := &Worker{
			now: func() time.Time { return now },
			resources: &fakeResourceStore{
				discoverFn: func(_ context.Context, parent *resources.Resource, _ oif.Document) error {
					discoveredUpstream = parent.UpstreamID
					return nil
				},
			},
			settlements: settlements,
		}
		payload := fmt.Sprintf(`{"id":"ftjob-1","status":"succeeded","trained_tokens":250,"finished_at":%q,"metadata":{%q:%q}}`,
			now.Add(-time.Minute).Format(time.RFC3339Nano), resources.RetainedSubmissionMarkerField, resourceUUID.String())
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload))}
		endpoint := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "fine_tuning_job", Identity: "id"}, Action: "get"}

		if class := worker.observePollResponse(t.Context(), settlements, resources.RetainedClaim{ResourceID: resourceUUID}, orig, endpoint, "ftjob-1", true, resp); class != "" {
			t.Fatalf("observePollResponse() = %q, want empty", class)
		}
		if orig.UpstreamID != "pending:sub-1" || orig.State != "submission_unknown" || !bytes.Equal(orig.Metadata, origMeta) {
			t.Fatalf("input resource was mutated: upstream=%q state=%q metadata=%s", orig.UpstreamID, orig.State, orig.Metadata)
		}
		if discoveredUpstream != "ftjob-1" {
			t.Fatalf("DiscoverRetainedModels saw upstream %q, want %q", discoveredUpstream, "ftjob-1")
		}
		if committed == nil || !committedAdopt || committed.UpstreamID != "ftjob-1" || committed.State != "succeeded" {
			t.Fatalf("unexpected committed resource: adopt=%t resource=%+v", committedAdopt, committed)
		}
		if !committedObs.CoverageComplete || len(committedObs.Lines) != 1 || committedObs.Lines[0].Usage.TrainingTokens == nil || *committedObs.Lines[0].Usage.TrainingTokens != 250 {
			t.Fatalf("unexpected observation: %+v", committedObs)
		}
	})

	t.Run("adopts vertex_tuning_job via labels without mutating input resource", func(t *testing.T) {
		t.Parallel()
		origMeta := json.RawMessage(`{"upstream_model":"gemini-2.5-flash"}`)
		orig := &resources.Resource{
			UUID:       resourceUUID,
			Kind:       "vertex_tuning_job",
			UpstreamID: "pending:sub-2",
			State:      "submission_unknown",
			Metadata:   slices.Clone(origMeta),
			CreatedAt:  createdAt,
		}
		var committed *resources.Resource
		settlements := &fakeSettlementStore{
			commitPollFn: func(_ context.Context, _ resources.RetainedClaim, r *resources.Resource, _ resources.RetainedObservation, adopt bool) error {
				if !adopt {
					t.Error("expected adopt=true")
				}
				copied := copyResource(r)
				committed = &copied
				return nil
			},
		}
		worker := &Worker{
			now:         func() time.Time { return now },
			resources:   &fakeResourceStore{},
			settlements: settlements,
		}
		upstream := "projects/p/locations/us-central1/tuningJobs/99"
		payload := fmt.Sprintf(`{"name":%q,"state":"JOB_STATE_RUNNING","labels":{%q:%q}}`,
			upstream, resources.RetainedSubmissionMarkerField, resourceUUID.String())
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload))}
		endpoint := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "vertex_tuning_job", Identity: "name"}, Action: "get"}

		if class := worker.observePollResponse(t.Context(), settlements, resources.RetainedClaim{ResourceID: resourceUUID}, orig, endpoint, upstream, true, resp); class != "" {
			t.Fatalf("observePollResponse() = %q, want empty", class)
		}
		if orig.UpstreamID != "pending:sub-2" || orig.State != "submission_unknown" || !bytes.Equal(orig.Metadata, origMeta) {
			t.Fatalf("input resource was mutated: upstream=%q state=%q", orig.UpstreamID, orig.State)
		}
		if committed == nil || committed.UpstreamID != upstream || committed.State != "JOB_STATE_RUNNING" {
			t.Fatalf("unexpected committed vertex resource: %+v", committed)
		}
	})

	t.Run("rejects wrong marker location and keeps input immutable on failure", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name        string
			kind        string
			identity    string
			upstream    string
			payload     string
			discoverErr error
			commitErr   error
			wantClass   string
		}{
			{
				name:      "vertex job marker in metadata instead of labels",
				kind:      "vertex_tuning_job",
				identity:  "name",
				upstream:  "projects/p/locations/l/tuningJobs/1",
				payload:   fmt.Sprintf(`{"name":"projects/p/locations/l/tuningJobs/1","metadata":{%q:%q}}`, resources.RetainedSubmissionMarkerField, resourceUUID.String()),
				wantClass: "uncorrelated_resource",
			},
			{
				name:      "openai job marker in labels instead of metadata",
				kind:      "fine_tuning_job",
				identity:  "id",
				upstream:  "ftjob-1",
				payload:   fmt.Sprintf(`{"id":"ftjob-1","labels":{%q:%q}}`, resources.RetainedSubmissionMarkerField, resourceUUID.String()),
				wantClass: "uncorrelated_resource",
			},
			{
				name:      "mismatched marker uuid",
				kind:      "fine_tuning_job",
				identity:  "id",
				upstream:  "ftjob-1",
				payload:   fmt.Sprintf(`{"id":"ftjob-1","metadata":{%q:%q}}`, resources.RetainedSubmissionMarkerField, uuid.NewString()),
				wantClass: "uncorrelated_resource",
			},
			{
				name:        "discovery failure does not mutate input resource",
				kind:        "fine_tuning_job",
				identity:    "id",
				upstream:    "ftjob-1",
				payload:     fmt.Sprintf(`{"id":"ftjob-1","status":"running","metadata":{%q:%q}}`, resources.RetainedSubmissionMarkerField, resourceUUID.String()),
				discoverErr: errors.New("db down"),
				wantClass:   "model_discovery_failed",
			},
			{
				name:      "commit failure does not mutate input resource",
				kind:      "fine_tuning_job",
				identity:  "id",
				upstream:  "ftjob-1",
				payload:   fmt.Sprintf(`{"id":"ftjob-1","status":"running","metadata":{%q:%q}}`, resources.RetainedSubmissionMarkerField, resourceUUID.String()),
				commitErr: errors.New("conflict"),
				wantClass: "settlement_failed",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				origMeta := json.RawMessage(`{"upstream_model":"gpt-4.1"}`)
				orig := &resources.Resource{
					UUID:       resourceUUID,
					Kind:       tc.kind,
					UpstreamID: "pending:orig",
					State:      "submission_unknown",
					Metadata:   slices.Clone(origMeta),
					CreatedAt:  createdAt,
				}
				var committed bool
				settlements := &fakeSettlementStore{
					commitPollFn: func(context.Context, resources.RetainedClaim, *resources.Resource, resources.RetainedObservation, bool) error {
						committed = true
						return tc.commitErr
					},
				}
				worker := &Worker{
					now: func() time.Time { return now },
					resources: &fakeResourceStore{
						discoverFn: func(context.Context, *resources.Resource, oif.Document) error {
							return tc.discoverErr
						},
					},
					settlements: settlements,
				}
				resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.payload))}
				endpoint := retainedapi.Endpoint{Family: retainedapi.Family{Kind: tc.kind, Identity: tc.identity}, Action: "get"}
				got := worker.observePollResponse(t.Context(), settlements, resources.RetainedClaim{ResourceID: resourceUUID}, orig, endpoint, tc.upstream, true, resp)
				if got != tc.wantClass {
					t.Fatalf("observePollResponse() = %q, want %q", got, tc.wantClass)
				}
				if orig.UpstreamID != "pending:orig" || orig.State != "submission_unknown" || !bytes.Equal(orig.Metadata, origMeta) {
					t.Fatalf("input resource was mutated on error %q: upstream=%q state=%q metadata=%s", got, orig.UpstreamID, orig.State, orig.Metadata)
				}
				if tc.commitErr == nil && committed {
					t.Fatal("CommitPoll should not have been called")
				}
			})
		}
	})
}

func TestObservePollResponseValidation(t *testing.T) {
	t.Parallel()

	endpoint := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "vector_store", Identity: "id"}, Action: "get"}
	baseResource := func() *resources.Resource {
		return &resources.Resource{
			UUID:       uuid.New(),
			Kind:       "vector_store",
			UpstreamID: "vs-1",
			State:      "in_progress",
			Metadata:   json.RawMessage(`{}`),
		}
	}

	t.Run("identity mismatch", func(t *testing.T) {
		t.Parallel()
		settlements := &fakeSettlementStore{}
		worker := &Worker{resources: &fakeResourceStore{}, settlements: settlements}
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"vs-other"}`))}
		if got := worker.observePollResponse(t.Context(), settlements, resources.RetainedClaim{}, baseResource(), endpoint, "vs-1", false, resp); got != "resource_identity_mismatch" {
			t.Fatalf("observePollResponse() = %q, want %q", got, "resource_identity_mismatch")
		}
	})

	t.Run("payload over 4 MiB rejected", func(t *testing.T) {
		t.Parallel()
		settlements := &fakeSettlementStore{}
		worker := &Worker{resources: &fakeResourceStore{}, settlements: settlements}
		oversized := `{"id":"vs-1","pad":"` + strings.Repeat("a", maxObservationBytes) + `"}`
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(oversized))}
		if got := worker.observePollResponse(t.Context(), settlements, resources.RetainedClaim{}, baseResource(), endpoint, "vs-1", false, resp); got != "invalid_observation" {
			t.Fatalf("observePollResponse() = %q, want %q", got, "invalid_observation")
		}
	})

	t.Run("non-2xx fails without double draining", func(t *testing.T) {
		t.Parallel()
		settlements := &fakeSettlementStore{}
		worker := &Worker{resources: &fakeResourceStore{}, settlements: settlements}

		body500 := newTrackingBody(bytes.Repeat([]byte("x"), 1024))
		resp := &http.Response{StatusCode: http.StatusInternalServerError, Body: body500}
		if got := worker.observePollResponse(t.Context(), settlements, resources.RetainedClaim{}, baseResource(), endpoint, "vs-1", false, resp); got != "poll_failed" {
			t.Fatalf("observePollResponse(500) = %q, want %q", got, "poll_failed")
		}
		if body500.bytesRead != 0 {
			t.Fatalf("decodePollResponse read %d bytes on non-2xx, want 0 (draining owned by poll defer)", body500.bytesRead)
		}

		body404 := newTrackingBody(bytes.Repeat([]byte("y"), maxDrainBytes*4))
		resp404 := &http.Response{StatusCode: http.StatusNotFound, Body: body404}
		if got := worker.observePollResponse(t.Context(), settlements, resources.RetainedClaim{}, baseResource(), endpoint, "vs-1", true, resp404); got != "poll_failed" {
			t.Fatalf("observePollResponse(404 adopt) = %q, want %q", got, "poll_failed")
		}
		if body404.bytesRead != 0 {
			t.Fatalf("decodePollResponse read %d bytes on 404 adopt, want 0", body404.bytesRead)
		}
	})
}

func TestDrainAndClose(t *testing.T) {
	t.Parallel()

	t.Run("nil body is safe", func(t *testing.T) {
		t.Parallel()
		drainAndClose(nil)
	})

	t.Run("drains normal body and closes", func(t *testing.T) {
		t.Parallel()
		body := newTrackingBody(bytes.Repeat([]byte("a"), 1024))
		drainAndClose(body)
		if body.bytesRead != 1024 || !body.closed {
			t.Fatalf("drainAndClose read=%d closed=%t, want 1024 and true", body.bytesRead, body.closed)
		}
	})

	t.Run("caps draining at 64 KiB and closes", func(t *testing.T) {
		t.Parallel()
		body := newTrackingBody(bytes.Repeat([]byte("b"), maxDrainBytes*4))
		drainAndClose(body)
		if body.bytesRead != maxDrainBytes || !body.closed {
			t.Fatalf("drainAndClose read=%d closed=%t, want %d and true", body.bytesRead, body.closed, maxDrainBytes)
		}
	})

	t.Run("drains and closes after non-2xx or 404 handling", func(t *testing.T) {
		t.Parallel()
		for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable} {
			body := newTrackingBody(bytes.Repeat([]byte("e"), 2048))
			resp := &http.Response{StatusCode: status, Body: body}
			func() {
				defer drainAndClose(resp.Body)
				_, _, errClass := decodePollResponse(retainedapi.Endpoint{Family: retainedapi.Family{Identity: "id"}}, "vs-1", resp)
				if errClass != "poll_failed" {
					t.Errorf("decodePollResponse(%d) = %q, want poll_failed", status, errClass)
				}
			}()
			if body.bytesRead != 2048 || !body.closed {
				t.Fatalf("status %d: read=%d closed=%t, want 2048 and true", status, body.bytesRead, body.closed)
			}
		}
	})
}

func TestHandleMissingRemote(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	t.Run("asynchronous job 404 leaves termination unverified", func(t *testing.T) {
		t.Parallel()
		var committed bool
		settlements := &fakeSettlementStore{
			commitPollFn: func(context.Context, resources.RetainedClaim, *resources.Resource, resources.RetainedObservation, bool) error {
				committed = true
				return nil
			},
		}
		worker := &Worker{now: func() time.Time { return now }, settlements: settlements}
		r := &resources.Resource{UUID: uuid.New(), Kind: "fine_tuning_job"}
		if got := worker.handleMissingRemote(t.Context(), settlements, resources.RetainedClaim{}, r); got != "remote_termination_unverified" {
			t.Fatalf("handleMissingRemote() = %q, want %q", got, "remote_termination_unverified")
		}
		if committed {
			t.Fatal("CommitPoll should not be called for asynchronous job 404")
		}
	})

	t.Run("expired context cache settles completely on 404", func(t *testing.T) {
		t.Parallel()
		created := now.Add(-2 * time.Hour)
		expired := now.Add(-time.Hour)
		meta := fmt.Sprintf(`{"upstream_model":"gemini-2.5-flash","total_token_count":1000,"create_time":%q,"expires_at":%q}`,
			created.Format(time.RFC3339Nano), expired.Format(time.RFC3339Nano))
		var committedObs resources.RetainedObservation
		settlements := &fakeSettlementStore{
			commitPollFn: func(_ context.Context, _ resources.RetainedClaim, _ *resources.Resource, obs resources.RetainedObservation, adopt bool) error {
				if adopt {
					t.Error("expected adopt=false")
				}
				committedObs = obs
				return nil
			},
		}
		worker := &Worker{now: func() time.Time { return now }, settlements: settlements}
		r := &resources.Resource{UUID: uuid.New(), Kind: "context_cache", State: "active", ExpiresAt: &expired, Metadata: json.RawMessage(meta)}
		if got := worker.handleMissingRemote(t.Context(), settlements, resources.RetainedClaim{ResourceID: r.UUID}, r); got != "" {
			t.Fatalf("handleMissingRemote() = %q, want empty", got)
		}
		if !committedObs.RemoteTerminated || !committedObs.CoverageComplete || len(committedObs.Lines) != 1 {
			t.Fatalf("unexpected expiry observation: %+v", committedObs)
		}
	})
}

func TestPassBoundedWavesConcurrencyAndImmediateHeartbeat(t *testing.T) {
	t.Parallel()

	const totalAvailable = 20
	pending := make([]resources.RetainedClaim, totalAvailable)
	for i := range pending {
		pending[i] = resources.RetainedClaim{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"}
	}

	var mu sync.Mutex
	offset := 0
	var claimCalls int
	claimed := map[uuid.UUID]bool{}
	finished := map[uuid.UUID]bool{}
	firstWave := map[uuid.UUID]bool{}
	firstWaveHeartbeats := map[uuid.UUID]int{}
	var heartbeatViolations []string
	var maxActive atomic.Int32
	var currentActive atomic.Int32
	var checkpoints atomic.Int32

	firstWaveReady := make(chan struct{})
	firstWaveReadyOnce := sync.Once{}
	releaseFirstWave := make(chan struct{})
	releaseFirstWaveOnce := sync.Once{}

	store := &fakeSettlementStore{
		claimFn: func(_ context.Context, limit int) ([]resources.RetainedClaim, error) {
			if limit > maxConcurrentClaims {
				t.Errorf("Claim limit = %d, exceeds maxConcurrentClaims %d", limit, maxConcurrentClaims)
			}
			mu.Lock()
			defer mu.Unlock()
			claimCalls++
			end := min(offset+limit, len(pending))
			batch := slices.Clone(pending[offset:end])
			offset = end
			for _, c := range batch {
				claimed[c.ResourceID] = true
			}
			return batch, nil
		},
		closeUndispatchedFn: func(_ context.Context, c resources.RetainedClaim) (bool, error) {
			cur := currentActive.Add(1)
			for {
				prev := maxActive.Load()
				if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
					break
				}
			}
			defer currentActive.Add(-1)

			mu.Lock()
			inFirstWave := len(firstWave) < maxConcurrentClaims
			if inFirstWave {
				firstWave[c.ResourceID] = true
				if len(firstWave) == maxConcurrentClaims {
					firstWaveReadyOnce.Do(func() { close(firstWaveReady) })
				}
			}
			mu.Unlock()

			if inFirstWave {
				<-firstWaveReady
				<-releaseFirstWave
			}
			return true, nil
		},
		heartbeatFn: func(_ context.Context, c resources.RetainedClaim) error {
			mu.Lock()
			defer mu.Unlock()
			if !claimed[c.ResourceID] {
				heartbeatViolations = append(heartbeatViolations, fmt.Sprintf("heartbeat for unclaimed %s", c.ResourceID))
			}
			if finished[c.ResourceID] {
				heartbeatViolations = append(heartbeatViolations, fmt.Sprintf("heartbeat after finish for %s", c.ResourceID))
			}
			select {
			case <-releaseFirstWave:
			default:
				if firstWave[c.ResourceID] {
					firstWaveHeartbeats[c.ResourceID]++
					if len(firstWaveHeartbeats) == maxConcurrentClaims {
						releaseFirstWaveOnce.Do(func() { close(releaseFirstWave) })
					}
				}
			}
			return nil
		},
		finishClaimFn: func(_ context.Context, c resources.RetainedClaim, errorClass string) error {
			if errorClass != "" {
				t.Errorf("FinishClaim(%s) errorClass = %q, want empty", c.ResourceID, errorClass)
			}
			mu.Lock()
			finished[c.ResourceID] = true
			mu.Unlock()
			return nil
		},
		checkpointFn: func(context.Context, workerhealth.Outcome) error {
			checkpoints.Add(1)
			return nil
		},
	}

	worker := &Worker{settlements: store, heartbeatInterval: 5 * time.Millisecond}
	if err := worker.Pass(t.Context()); err != nil {
		t.Fatalf("Pass() error = %v", err)
	}
	if got := maxActive.Load(); got != maxConcurrentClaims {
		t.Fatalf("max concurrent active claims = %d, want %d", got, maxConcurrentClaims)
	}
	if got := checkpoints.Load(); got != 0 {
		t.Fatalf("Pass() called Checkpoint %d times, want 0", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if claimCalls != maxWavesPerPass {
		t.Fatalf("Claim called %d times, want bounded %d waves", claimCalls, maxWavesPerPass)
	}
	if len(finished) != maxClaimsPerPass {
		t.Fatalf("finished %d claims, want bounded %d per pass", len(finished), maxClaimsPerPass)
	}
	if len(firstWaveHeartbeats) != maxConcurrentClaims {
		t.Fatalf("first wave heartbeated %d claims, want all %d active claims", len(firstWaveHeartbeats), maxConcurrentClaims)
	}
	if len(heartbeatViolations) > 0 {
		t.Fatalf("heartbeat lifecycle violations: %v", heartbeatViolations)
	}
}

func TestPassStopsOnPartialWaveAndJoinsWaveErrors(t *testing.T) {
	t.Parallel()

	pending := make([]resources.RetainedClaim, 6)
	for i := range pending {
		pending[i] = resources.RetainedClaim{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"}
	}
	failWave1 := pending[1].ResourceID
	failWave2 := pending[5].ResourceID

	var mu sync.Mutex
	offset := 0
	var claimCalls int
	finished := map[uuid.UUID]string{}

	store := &fakeSettlementStore{
		claimFn: func(_ context.Context, limit int) ([]resources.RetainedClaim, error) {
			mu.Lock()
			defer mu.Unlock()
			claimCalls++
			end := min(offset+limit, len(pending))
			batch := slices.Clone(pending[offset:end])
			offset = end
			return batch, nil
		},
		closeUndispatchedFn: func(_ context.Context, c resources.RetainedClaim) (bool, error) {
			if c.ResourceID == failWave1 {
				return false, nil
			}
			if c.ResourceID == failWave2 {
				return false, errors.New("lease lost")
			}
			return true, nil
		},
		resourceFn: func(context.Context, resources.RetainedClaim) (*resources.Resource, error) {
			return nil, errors.New("missing row")
		},
		finishClaimFn: func(_ context.Context, c resources.RetainedClaim, errorClass string) error {
			mu.Lock()
			finished[c.ResourceID] = errorClass
			mu.Unlock()
			return nil
		},
	}

	worker := &Worker{settlements: store, heartbeatInterval: time.Hour}
	err := worker.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "resource_unavailable") || !strings.Contains(err.Error(), "claim_unavailable") {
		t.Fatalf("Pass() error = %v, want both resource_unavailable and claim_unavailable", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if claimCalls != 2 {
		t.Fatalf("Claim called %d times, want 2 (stop on partial wave)", claimCalls)
	}
	if len(finished) != len(pending) {
		t.Fatalf("finished %d claims, want %d", len(finished), len(pending))
	}
	if finished[failWave1] != "resource_unavailable" || finished[failWave2] != "claim_unavailable" {
		t.Fatalf("unexpected finish classes: %v", finished)
	}
}

func TestRunFailedClaimNeverCheckpointsSuccess(t *testing.T) {
	t.Parallel()

	okClaim := resources.RetainedClaim{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"}
	failClaim := resources.RetainedClaim{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"}

	var mu sync.Mutex
	finishedClasses := map[uuid.UUID]string{}
	var outcomes []workerhealth.Outcome
	var heartbeats atomic.Int32
	twoHeartbeats := make(chan struct{})
	twoHeartbeatsOnce := sync.Once{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := &fakeSettlementStore{
		claimFn: func(context.Context, int) ([]resources.RetainedClaim, error) {
			return []resources.RetainedClaim{okClaim, failClaim}, nil
		},
		closeUndispatchedFn: func(_ context.Context, c resources.RetainedClaim) (bool, error) {
			if c.ResourceID == okClaim.ResourceID {
				return true, nil
			}
			<-twoHeartbeats
			return false, nil
		},
		resourceFn: func(context.Context, resources.RetainedClaim) (*resources.Resource, error) {
			return nil, errors.New("resource lookup failed")
		},
		heartbeatFn: func(_ context.Context, c resources.RetainedClaim) error {
			if c.ResourceID == failClaim.ResourceID && heartbeats.Add(1) >= 2 {
				twoHeartbeatsOnce.Do(func() { close(twoHeartbeats) })
			}
			return nil
		},
		finishClaimFn: func(_ context.Context, c resources.RetainedClaim, errorClass string) error {
			mu.Lock()
			finishedClasses[c.ResourceID] = errorClass
			mu.Unlock()
			return nil
		},
		checkpointFn: func(_ context.Context, outcome workerhealth.Outcome) error {
			mu.Lock()
			outcomes = append(outcomes, outcome)
			mu.Unlock()
			cancel()
			return nil
		},
	}

	worker := &Worker{settlements: store, heartbeatInterval: 5 * time.Millisecond}
	worker.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if heartbeats.Load() < 2 {
		t.Fatalf("expected at least 2 heartbeats on slow failing claim, got %d", heartbeats.Load())
	}
	if finishedClasses[okClaim.ResourceID] != "" {
		t.Fatalf("ok claim errorClass = %q, want empty", finishedClasses[okClaim.ResourceID])
	}
	if finishedClasses[failClaim.ResourceID] != "resource_unavailable" {
		t.Fatalf("failing claim errorClass = %q, want %q", finishedClasses[failClaim.ResourceID], "resource_unavailable")
	}
	if len(outcomes) != 1 || outcomes[0] != workerhealth.OutcomeFailure {
		t.Fatalf("checkpoint outcomes = %v, want single OutcomeFailure", outcomes)
	}
}

func TestPassHeartbeatFailureCancelsActiveClaim(t *testing.T) {
	t.Parallel()

	claim := resources.RetainedClaim{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"}
	var finishCtxErr error
	var finishClass string

	store := &fakeSettlementStore{
		claimFn: func(context.Context, int) ([]resources.RetainedClaim, error) {
			return []resources.RetainedClaim{claim}, nil
		},
		closeUndispatchedFn: func(ctx context.Context, _ resources.RetainedClaim) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		},
		heartbeatFn: func(context.Context, resources.RetainedClaim) error {
			return resources.ErrSubmissionConflict
		},
		finishClaimFn: func(ctx context.Context, _ resources.RetainedClaim, errorClass string) error {
			finishCtxErr = ctx.Err()
			finishClass = errorClass
			return nil
		},
	}

	worker := &Worker{settlements: store, heartbeatInterval: 5 * time.Millisecond}
	err := worker.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "claim_unavailable") {
		t.Fatalf("Pass() error = %v, want claim_unavailable", err)
	}
	if finishCtxErr != nil || finishClass != "claim_unavailable" {
		t.Fatalf("FinishClaim ctxErr=%v class=%q, want nil and claim_unavailable", finishCtxErr, finishClass)
	}
}

func TestPassCancellationStopsFurtherWavesAndReleasesClaimed(t *testing.T) {
	t.Parallel()

	t.Run("pre-cancelled context returns ctx error without claiming", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		var claimCalls int
		store := &fakeSettlementStore{
			claimFn: func(context.Context, int) ([]resources.RetainedClaim, error) {
				claimCalls++
				return nil, nil
			},
		}

		worker := &Worker{settlements: store, heartbeatInterval: time.Hour}
		if err := worker.Pass(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Pass(cancelled) error = %v, want context.Canceled", err)
		}
		if claimCalls != 0 {
			t.Fatalf("Claim called %d times on pre-cancelled context, want 0", claimCalls)
		}
	})

	t.Run("stops before claiming next wave", func(t *testing.T) {
		t.Parallel()
		pending := make([]resources.RetainedClaim, 8)
		for i := range pending {
			pending[i] = resources.RetainedClaim{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"}
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var started atomic.Int32
		var mu sync.Mutex
		offset := 0
		var claimCalls int
		finished := map[uuid.UUID]string{}
		var finishCtxErrors int

		store := &fakeSettlementStore{
			claimFn: func(_ context.Context, limit int) ([]resources.RetainedClaim, error) {
				mu.Lock()
				defer mu.Unlock()
				claimCalls++
				end := min(offset+limit, len(pending))
				batch := slices.Clone(pending[offset:end])
				offset = end
				return batch, nil
			},
			closeUndispatchedFn: func(work context.Context, _ resources.RetainedClaim) (bool, error) {
				if started.Add(1) == maxConcurrentClaims {
					cancel()
				}
				<-work.Done()
				return false, work.Err()
			},
			finishClaimFn: func(finishCtx context.Context, c resources.RetainedClaim, errorClass string) error {
				mu.Lock()
				defer mu.Unlock()
				if finishCtx.Err() != nil {
					finishCtxErrors++
				}
				finished[c.ResourceID] = errorClass
				return nil
			},
		}

		worker := &Worker{settlements: store, heartbeatInterval: time.Hour}
		if err := worker.Pass(ctx); err == nil {
			t.Fatal("Pass() succeeded after cancellation, want error")
		}
		mu.Lock()
		defer mu.Unlock()
		if claimCalls != 1 {
			t.Fatalf("Claim called %d times, want 1 (never claim next wave after cancel)", claimCalls)
		}
		if len(finished) != maxConcurrentClaims {
			t.Fatalf("finished %d claims, want %d", len(finished), maxConcurrentClaims)
		}
		if finishCtxErrors != 0 {
			t.Fatalf("FinishClaim received cancelled context %d times, want 0", finishCtxErrors)
		}
	})

	t.Run("releases claims cancelled right after claim returns", func(t *testing.T) {
		t.Parallel()
		claims := []resources.RetainedClaim{
			{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"},
			{ResourceID: uuid.New(), ClaimID: uuid.New(), Operation: "fine_tuning"},
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var mu sync.Mutex
		finished := map[uuid.UUID]string{}
		store := &fakeSettlementStore{
			claimFn: func(context.Context, int) ([]resources.RetainedClaim, error) {
				cancel()
				return claims, nil
			},
			closeUndispatchedFn: func(context.Context, resources.RetainedClaim) (bool, error) {
				t.Error("CloseUndispatched should not run after context cancellation")
				return false, nil
			},
			finishClaimFn: func(finishCtx context.Context, c resources.RetainedClaim, errorClass string) error {
				if finishCtx.Err() != nil {
					t.Errorf("FinishClaim received cancelled context: %v", finishCtx.Err())
				}
				mu.Lock()
				finished[c.ResourceID] = errorClass
				mu.Unlock()
				return nil
			},
		}

		worker := &Worker{settlements: store, heartbeatInterval: time.Hour}
		if err := worker.Pass(ctx); err == nil || !strings.Contains(err.Error(), "poll_cancelled") {
			t.Fatalf("Pass() error = %v, want poll_cancelled", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, c := range claims {
			if finished[c.ResourceID] != "poll_cancelled" {
				t.Fatalf("claim %s finished with %q, want poll_cancelled", c.ResourceID, finished[c.ResourceID])
			}
		}
	})
}

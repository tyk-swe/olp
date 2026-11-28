//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/internal/usage/reports"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestRequestDetailKeepsTheObservedCacheLookupReason(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	event := fixture.event(fixture.attempt(1))
	event.CacheResult, event.CacheLookupReason = "bypass", usage.CacheLookupUnavailable
	record, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []ingest.PersistOutcome{ingest.PersistOutcomePersisted, ingest.PersistOutcomeDuplicate} {
		if result, err := ingest.PersistEvent(ctx, pool, record); err != nil || result.Outcome != want {
			t.Fatalf("delivery outcome=%v error=%v, want %v", result.Outcome, err, want)
		}
	}
	altered := *event
	altered.CacheLookupReason = usage.CacheLookupNoStore
	conflict, err := usage.NewAccountingEvent(&altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.PersistEvent(ctx, pool, conflict); !errors.Is(err, usage.ErrInvalidEvent) {
		t.Fatalf("persisting an altered replay: %v", err)
	}
	legacy := fixture.event(fixture.attempt(1))
	legacy.CacheResult = "bypass"
	old, err := usage.NewAccountingEvent(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.PersistEvent(ctx, pool, old); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM olp.requests WHERE id=$1", event.RequestID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("requests after replay=%d, error=%v", count, err)
	}
	requestReport := requestReports(t, fixture)
	for _, tc := range []struct {
		token, id string
		status    int
		reason    usage.CacheLookupReason
	}{
		{"owner-session", event.RequestID, http.StatusOK, usage.CacheLookupUnavailable},
		{"owner-session", legacy.RequestID, http.StatusOK, ""},
		{"other-project-session", event.RequestID, http.StatusNotFound, ""},
	} {
		status, body := requestReport(tc.token, "/"+tc.id)
		if status != tc.status {
			t.Fatalf("request detail status=%d, want %d: %s", status, tc.status, body)
		}
		if tc.status == http.StatusOK {
			var detail struct {
				Result string                  `json:"cache_result"`
				Reason usage.CacheLookupReason `json:"cache_lookup_reason"`
			}
			if err := json.Unmarshal(body, &detail); err != nil || detail.Result != "bypass" || detail.Reason != tc.reason {
				t.Fatalf("unexpected cache detail: %s (decode error: %v)", body, err)
			}
		} else if bytes.Contains(body, []byte(usage.CacheLookupUnavailable)) {
			t.Fatalf("another project saw the reason: %s", body)
		}
	}
}

func TestRequestDetailKeepsProviderRequestIDsOnReplayAndWithinProject(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	failed, succeeded := fixture.attempt(1), fixture.attempt(2)
	failureStatus := http.StatusServiceUnavailable
	failed.StatusCode, failed.Committed = &failureStatus, false
	failed.Usage = &usage.AttemptUsage{Evidence: usage.EvidenceUnobservedComplete}
	failed.ProviderRequestID, succeeded.ProviderRequestID = "req_Unavailable123", "req_Served456"
	event := fixture.event(failed, succeeded)
	requestReport := requestReports(t, fixture)

	record, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []ingest.PersistOutcome{ingest.PersistOutcomePersisted, ingest.PersistOutcomeDuplicate} {
		if result, err := ingest.PersistEvent(t.Context(), pool, record); err != nil || result.Outcome != want {
			t.Fatalf("delivery outcome=%v error=%v, want %v", result.Outcome, err, want)
		}
	}
	altered := *event
	replacement := failed
	replacement.ID, replacement.ProviderRequestID = uuid.NewString(), succeeded.ProviderRequestID
	altered.Attempts = []usage.Attempt{replacement, succeeded}
	conflict, err := usage.NewAccountingEvent(&altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.PersistEvent(t.Context(), pool, conflict); !errors.Is(err, usage.ErrInvalidEvent) {
		t.Fatalf("persisting an altered replay: %v", err)
	}

	status, body := requestReport("owner-session", "/"+event.RequestID)
	var detail reports.RequestDetail
	if err := json.Unmarshal(body, &detail); status != http.StatusOK || err != nil || detail.ID != event.RequestID || len(detail.Attempts) != 2 {
		t.Fatalf("unexpected request detail: %d %s (decode error: %v)", status, body, err)
	}
	for i, want := range []usage.Attempt{failed, succeeded} {
		got := detail.Attempts[i]
		if got.ID != want.ID || got.ProviderRequestID == nil || *got.ProviderRequestID != want.ProviderRequestID {
			t.Fatalf("attempt %d changed after replay: %s", i+1, body)
		}
	}
	for _, tc := range []struct {
		token, path string
		status      int
	}{
		{"other-project-session", "/" + event.RequestID, http.StatusNotFound},
		{"owner-session", "", http.StatusOK},
		{"owner-session", "/export.csv", http.StatusOK},
	} {
		status, body := requestReport(tc.token, tc.path)
		if status != tc.status || bytes.Contains(body, []byte(failed.ProviderRequestID)) || bytes.Contains(body, []byte(succeeded.ProviderRequestID)) {
			t.Fatalf("%s %s returned %d and %s", tc.token, tc.path, status, body)
		}
	}
}

func requestReports(t *testing.T, fixture usageFixture) func(string, string) (int, []byte) {
	t.Helper()
	ctx, pool := t.Context(), fixture.pool
	otherUser, otherProject := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.users (id,email,display_name,role,etag,access_scope)
		VALUES ($1::uuid,'other@example.com','Other project','operator',$1::uuid,'assigned')`, otherUser); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.projects (id,name,etag,created_by)
		VALUES ($1::uuid,'other',$1::uuid,$2)`, otherProject, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.project_members (project_id,user_id,role,added_by)
		VALUES ($1,$2,'manager',$3)`, otherProject, otherUser, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	owner := managementHTTPAs(t, pool, fixture.ownerID, func(mux *http.ServeMux, server *access.Server) {
		(&reports.API{Access: server}).Register(mux)
	})
	clients := map[string]*managementClient{
		"owner-session":         owner,
		"other-project-session": owner.as(t, otherUser),
	}
	return func(token, suffix string) (int, []byte) {
		t.Helper()
		res := clients[token].call(t, http.MethodGet, "/api/v1/requests"+suffix, "", "")
		return res.status, res.body
	}
}

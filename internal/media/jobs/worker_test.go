package jobs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/upstream"
)

func TestScheduleBackoffAndTerminal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	svc := New(Deps{Now: func() time.Time { return now }})

	backoffCases := []struct {
		attempts int
		want     time.Duration
	}{
		{-2, 5 * time.Second},
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 160 * time.Second},
		{6, 300 * time.Second},
		{20, 300 * time.Second},
	}
	for _, tc := range backoffCases {
		next, failure := svc.schedule(Record{Lifecycle: Active, State: Running}, tc.attempts, errors.New("upstream_timeout"))
		if !next.Equal(now.Add(tc.want)) || failure == nil || *failure != "upstream_timeout" {
			t.Errorf("schedule(attempts=%d) = (%v, %v), want (%v, upstream_timeout)", tc.attempts, next, failure, now.Add(tc.want))
		}
	}

	longMsg := strings.Repeat("x", 200)
	_, truncated := svc.schedule(Record{}, 0, errors.New(longMsg))
	if truncated == nil || len(*truncated) != 120 {
		t.Fatalf("truncated failure length = %v, want 120", truncated)
	}

	_, fallback := svc.schedule(Record{}, 0, errors.New(""))
	if fallback == nil || *fallback != "reconciliation_failed" {
		t.Fatalf("empty error failure = %v, want reconciliation_failed", fallback)
	}

	nextPoll, failure := svc.schedule(Record{Lifecycle: Active, State: Running}, 3, nil)
	if failure != nil || !nextPoll.Equal(now.Add(pollInterval)) {
		t.Fatalf("polling schedule = (%v, %v), want (%v, nil)", nextPoll, failure, now.Add(pollInterval))
	}

	nextDeleted, failure := svc.schedule(Record{Lifecycle: Deleted, State: Succeeded, CreatedAt: now.Add(-40 * 24 * time.Hour)}, 0, nil)
	if failure != nil || !nextDeleted.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("deleted schedule = (%v, %v), want +24h", nextDeleted, failure)
	}

	soonExpiry := now.Add(2 * time.Hour)
	nextExpiry, _ := svc.schedule(Record{Lifecycle: Active, State: Succeeded, CreatedAt: now, ExpiresAt: &soonExpiry}, 0, nil)
	if !nextExpiry.Equal(soonExpiry) {
		t.Fatalf("expires schedule = %v, want %v", nextExpiry, soonExpiry)
	}

	pastExpiry := now.Add(-time.Minute)
	nextPast, _ := svc.schedule(Record{Lifecycle: Active, State: Succeeded, CreatedAt: now, ExpiresAt: &pastExpiry}, 0, nil)
	if !nextPast.Equal(now) {
		t.Fatalf("past expiry schedule = %v, want %v", nextPast, now)
	}

	nearRetention := now.Add(-retention).Add(3 * time.Hour)
	nextRetention, _ := svc.schedule(Record{Lifecycle: Active, State: Succeeded, CreatedAt: nearRetention}, 0, nil)
	if !nextRetention.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("retention schedule = %v, want +3h", nextRetention)
	}

	farRetention := now.Add(-time.Hour)
	nextCapped, _ := svc.schedule(Record{Lifecycle: Active, State: Succeeded, CreatedAt: farRetention}, 0, nil)
	if !nextCapped.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("far retention schedule = %v, want +24h", nextCapped)
	}
}

func TestRefreshableAndRetired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	svc := New(Deps{Now: func() time.Time { return now }})

	refreshCases := []struct {
		name   string
		record Record
		want   bool
	}{
		{"ambiguous", Record{Lifecycle: CreateAmbiguous}, true},
		{"cleanup", Record{Lifecycle: CleanupPending}, true},
		{"delete pending", Record{Lifecycle: DeletePending}, true},
		{"creating not due", Record{Lifecycle: Creating, NextReconciliationAt: now.Add(time.Second)}, false},
		{"creating due", Record{Lifecycle: Creating, NextReconciliationAt: now}, true},
		{"active queued with upstream", Record{Lifecycle: Active, State: Queued, UpstreamJobID: new("vid_1")}, true},
		{"active running without upstream", Record{Lifecycle: Active, State: Running}, false},
		{"active succeeded", Record{Lifecycle: Active, State: Succeeded, UpstreamJobID: new("vid_1")}, false},
		{"deleted", Record{Lifecycle: Deleted, State: Running, UpstreamJobID: new("vid_1")}, false},
	}
	for _, tc := range refreshCases {
		if got := refreshable(tc.record, now); got != tc.want {
			t.Errorf("refreshable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	past := now.Add(-time.Second)
	future := now.Add(time.Second)
	retiredCases := []struct {
		name   string
		record Record
		want   bool
	}{
		{"fresh", Record{CreatedAt: now}, false},
		{"expires in future", Record{CreatedAt: now, ExpiresAt: &future}, false},
		{"expires at now", Record{CreatedAt: now, ExpiresAt: &now}, true},
		{"expired in past", Record{CreatedAt: now, ExpiresAt: &past}, true},
		{"at retention boundary", Record{CreatedAt: now.Add(-retention)}, false},
		{"past retention", Record{CreatedAt: now.Add(-retention - time.Second)}, true},
	}
	for _, tc := range retiredCases {
		if got := svc.retired(tc.record); got != tc.want {
			t.Errorf("retired(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFailureCodeMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		class upstream.Class
		want  string
	}{
		{upstream.Protocol, "provider_protocol_error"},
		{upstream.Timeout, string(upstream.Timeout)},
		{upstream.RateLimit, string(upstream.RateLimit)},
		{upstream.ServerError, string(upstream.ServerError)},
		{upstream.ClientError, string(upstream.ClientError)},
		{upstream.Credential, string(upstream.Credential)},
		{upstream.Cancelled, string(upstream.Cancelled)},
		{upstream.Connect, "connect"},
	}
	for _, tc := range cases {
		got := failureCode(upstreamFailure{&transport.Failure{Class: tc.class}})
		if got.Error() != tc.want {
			t.Errorf("failureCode(%q) = %q, want %q", tc.class, got.Error(), tc.want)
		}
	}

	other := &Unavailable{Code: "media_job_runtime_unavailable"}
	if got := failureCode(other); !errors.Is(got, other) {
		t.Fatalf("failureCode(non-upstream) = %v, want %v", got, other)
	}
}

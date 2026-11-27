package jobs

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/media"
)

func TestParseLifecycleAndState(t *testing.T) {
	t.Parallel()

	for _, want := range []Lifecycle{Creating, Active, CreateAmbiguous, CleanupPending, DeletePending, Deleted} {
		got, ok := ParseLifecycle(string(want))
		if !ok || got != want {
			t.Errorf("ParseLifecycle(%q) = (%q, %v), want (%q, true)", want, got, ok, want)
		}
	}
	for _, invalid := range []string{"", "ACTIVE", " active", "pending", "unknown"} {
		if got, ok := ParseLifecycle(invalid); ok {
			t.Errorf("ParseLifecycle(%q) = (%q, true), want false", invalid, got)
		}
	}

	polling := map[State]bool{
		Queued:    true,
		Running:   true,
		Succeeded: false,
		Failed:    false,
		Cancelled: false,
	}
	for want, wantPolling := range polling {
		got, ok := ParseState(string(want))
		if !ok || got != want {
			t.Errorf("ParseState(%q) = (%q, %v), want (%q, true)", want, got, ok, want)
		}
		if got.Polling() != wantPolling {
			t.Errorf("State(%q).Polling() = %v, want %v", want, got.Polling(), wantPolling)
		}
	}
	for _, invalid := range []string{"", "completed", "in_progress", "QUEUED", "unknown"} {
		if got, ok := ParseState(invalid); ok {
			t.Errorf("ParseState(%q) = (%q, true), want false", invalid, got)
		}
	}
}

func TestRecordHelpers(t *testing.T) {
	t.Parallel()

	contentCases := []struct {
		state     State
		lifecycle Lifecycle
		want      bool
	}{
		{Succeeded, Active, true},
		{Succeeded, DeletePending, true},
		{Succeeded, Deleted, false},
		{Running, Active, false},
		{Queued, Creating, false},
		{Failed, Active, false},
		{Cancelled, Active, false},
	}
	for _, tc := range contentCases {
		r := Record{State: tc.state, Lifecycle: tc.lifecycle}
		if got := r.ContentAvailable(); got != tc.want {
			t.Errorf("Record{State:%q, Lifecycle:%q}.ContentAvailable() = %v, want %v", tc.state, tc.lifecycle, got, tc.want)
		}
	}

	if id, ok := (Record{}).Upstream(); ok || id != "" {
		t.Errorf("nil Upstream() = (%q, %v), want (\"\", false)", id, ok)
	}
	for _, bad := range []string{"", " ", " vid_1", "vid_1\n", "vid\x001", strings.Repeat("a", 1025)} {
		r := Record{UpstreamJobID: new(bad)}
		if id, ok := r.Upstream(); ok || id != "" {
			t.Errorf("Upstream(%q) = (%q, %v), want (\"\", false)", bad, id, ok)
		}
	}
	r := Record{UpstreamJobID: new("vid_123")}
	if id, ok := r.Upstream(); !ok || id != "vid_123" {
		t.Errorf("Upstream(vid_123) = (%q, %v), want (\"vid_123\", true)", id, ok)
	}
}

func TestObserve(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	if _, ok := Observe(nil, now); ok {
		t.Fatal("Observe(nil) succeeded, want false")
	}
	if _, ok := Observe(&media.VideoJobResult{Status: "unknown"}, now); ok {
		t.Fatal("Observe(unknown status) succeeded, want false")
	}

	for _, badProgress := range []float32{-0.1, 100.1, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		if _, ok := Observe(&media.VideoJobResult{Status: "in_progress", Progress: &badProgress}, now); ok {
			t.Errorf("Observe(progress=%v) succeeded, want false", badProgress)
		}
	}

	progress := float32(42.5)
	expires := int64(1777636800)
	errCode := "content_policy_violation"

	running, ok := Observe(&media.VideoJobResult{
		Status:    "in_progress",
		Progress:  &progress,
		ExpiresAt: &expires,
		ErrorCode: &errCode,
	}, now)
	if !ok {
		t.Fatal("Observe(in_progress) failed")
	}
	if running.state != Running || running.progress == nil || *running.progress != 42.5 ||
		running.expires == nil || running.expires.Unix() != expires || running.failure != nil || !running.polled.Equal(now) {
		t.Fatalf("Observe(in_progress) = %+v", running)
	}

	failed, ok := Observe(&media.VideoJobResult{
		Status:    "failed",
		ErrorCode: &errCode,
	}, now)
	if !ok || failed.state != Failed || failed.failure == nil || *failed.failure != errCode {
		t.Fatalf("Observe(failed) = %+v, %v", failed, ok)
	}

	zero := float32(0)
	hundred := float32(100)
	if u, ok := Observe(&media.VideoJobResult{Status: "queued", Progress: &zero}, now); !ok || u.state != Queued || *u.progress != 0 {
		t.Fatalf("Observe(queued, 0) = %+v, %v", u, ok)
	}
	if u, ok := Observe(&media.VideoJobResult{Status: "completed", Progress: &hundred}, now); !ok || u.state != Succeeded || *u.progress != 100 {
		t.Fatalf("Observe(completed, 100) = %+v, %v", u, ok)
	}
}

func TestScopeAndErrorHelpers(t *testing.T) {
	t.Parallel()

	if Everything.param() != nil {
		t.Fatalf("Everything.param() = %v, want nil", Everything.param())
	}
	empty := Only(nil)
	if empty.param() == nil || len(empty.param()) != 0 {
		t.Fatalf("Only(nil).param() = %#v, want non-nil empty slice", empty.param())
	}
	source := []string{"a", "b"}
	scoped := Only(source)
	source[0] = "mutated"
	if got := scoped.param(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Only() did not isolate caller slice: %v", got)
	}

	inner := errors.New("boom")
	wrapped := &Error{Kind: KindDatabase, Message: "store down", Err: inner}
	if wrapped.Error() != "store down: boom" || !errors.Is(wrapped, inner) {
		t.Fatalf("wrapped Error = %q, unwrap=%v", wrapped.Error(), errors.Unwrap(wrapped))
	}
	if kind, ok := KindOf(wrapped); !ok || kind != KindDatabase {
		t.Fatalf("KindOf(wrapped) = (%v, %v), want (KindDatabase, true)", kind, ok)
	}
	if plain := (&Error{Kind: KindBusy, Message: "busy"}); plain.Error() != "busy" {
		t.Fatalf("plain.Error() = %q, want busy", plain.Error())
	}
	if _, ok := KindOf(inner); ok {
		t.Fatal("KindOf(non-job error) = true, want false")
	}
	if u := (&Unavailable{Code: "media_job_runtime_unavailable"}); u.Error() != "media_job_runtime_unavailable" {
		t.Fatalf("Unavailable.Error() = %q", u.Error())
	}
}

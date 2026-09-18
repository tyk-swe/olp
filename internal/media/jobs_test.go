package media

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTrip(t *testing.T) {
	cursor := &Cursor{At: time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC), ID: uuid.Must(uuid.NewV7()).String()}
	decoded, err := DecodeCursor(cursor.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.At.Equal(cursor.At) || decoded.ID != cursor.ID {
		t.Fatalf("cursor mismatch: %+v", decoded)
	}
	for _, bad := range []string{"", "no-separator", "notatime|" + cursor.ID, "2026-03-01|not-a-uuid"} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Fatalf("accepted cursor %q", bad)
		}
	}
}

func TestAllowsRefreshTransition(t *testing.T) {
	cases := []struct {
		current, incoming State
		allowed           bool
	}{
		{StateQueued, StateRunning, true},
		{StateQueued, StateQueued, true},
		{StateRunning, StateSucceeded, true},
		{StateRunning, StateQueued, false},
		{StateSucceeded, StateRunning, false},
		{StateSucceeded, StateSucceeded, true},
		{StateFailed, StateSucceeded, false},
		{StateFailed, StateFailed, true},
		{StateCancelled, StateRunning, false},
	}
	for _, tc := range cases {
		if AllowsRefreshTransition(tc.current, tc.incoming) != tc.allowed {
			t.Fatalf("%s -> %s: want %v", tc.current, tc.incoming, tc.allowed)
		}
	}
}

func TestValidateUpdateBounds(t *testing.T) {
	if err := validateUpdate(JobUpdate{State: StateSucceeded, ContentAvailable: true}); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdate(JobUpdate{State: StateRunning, ContentAvailable: true}); err == nil {
		t.Fatal("content on non-succeeded state accepted")
	}
	failure := "provider_error"
	if err := validateUpdate(JobUpdate{State: StateFailed, ErrorClass: &failure}); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdate(JobUpdate{State: StateRunning, ErrorClass: &failure}); err == nil {
		t.Fatal("error class on non-failed state accepted")
	}
	over := float32(101)
	if err := validateUpdate(JobUpdate{State: StateRunning, ProgressPercent: &over}); err == nil {
		t.Fatal("progress overflow accepted")
	}
}

func TestJobHTTPErrorMapping(t *testing.T) {
	notFound := JobHTTPError(&JobError{Kind: JobErrorNotFound})
	if notFound.Status != http.StatusNotFound {
		t.Fatalf("not found status: %d", notFound.Status)
	}
	precondition := JobHTTPError(&JobError{Kind: JobErrorPrecondition})
	if precondition.Status != http.StatusConflict {
		t.Fatalf("precondition status: %d", precondition.Status)
	}
	conflict := JobHTTPError(&JobError{Kind: JobErrorUpstreamIdentityConflict})
	if conflict.Status != http.StatusConflict {
		t.Fatalf("conflict status: %d", conflict.Status)
	}
	if got := JobHTTPError(errors.New("boom")); got.Status != http.StatusServiceUnavailable {
		t.Fatalf("database error status: %d", got.Status)
	}
}

func TestLifecycleNeedsReconciliation(t *testing.T) {
	for _, l := range []Lifecycle{LifecycleCreating, LifecycleCreateAmbiguous, LifecycleCreateCleanupPending, LifecycleDeletePending} {
		if !l.NeedsReconciliation() {
			t.Fatalf("%s should need reconciliation", l)
		}
	}
	for _, l := range []Lifecycle{LifecycleActive, LifecycleDeleted} {
		if l.NeedsReconciliation() {
			t.Fatalf("%s should not need reconciliation", l)
		}
	}
}

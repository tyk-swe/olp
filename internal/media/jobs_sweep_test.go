//go:build integration

package media

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func (f *claimFixture) reserve(t *testing.T, staleAfter time.Time) JobRecord {
	t.Helper()
	record, err := ReserveJob(t.Context(), f.pool, Reservation{
		ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID,
		APIKeyID: f.apiKeyID, ProviderID: f.providerID, UpstreamModel: claimVideoModel,
		RouteSlug: "video-default", Operation: OpVideoCreate, Surface: "openai", SlotID: f.slotID,
		StaleAfter: staleAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func claimedIDs(t *testing.T, f *claimFixture, at time.Time) []string {
	t.Helper()
	records, err := ClaimJobs(t.Context(), f.pool, at, 32)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

// A create dispatch may legitimately outlast five minutes; the reconciler
// must not treat the reservation as abandoned until its dispatch window ends.
func TestIntegrationCreatingJobWaitsForItsDispatchWindow(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()
	now := time.Now()

	long := f.reserve(t, now.Add(20*time.Minute))
	if ids := claimedIDs(t, f, now.Add(6*time.Minute)); slices.Contains(ids, long.ID) {
		t.Fatal("in-flight create was claimed before its dispatch window closed")
	}
	summary, err := ReconciliationSummary(ctx, f.pool, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Stale != 0 {
		t.Fatalf("in-flight create counted as stale: %+v", summary)
	}
	attached, err := AttachUpstream(ctx, f.pool, long.ID, "upstream-video-slow",
		JobUpdate{State: StateQueued, LastPolledAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if attached.Lifecycle != LifecycleActive {
		t.Fatalf("attached lifecycle %q", attached.Lifecycle)
	}

	// An abandoned create is still recovered once its window has passed.
	short := f.reserve(t, now.Add(time.Minute))
	if ids := claimedIDs(t, f, now.Add(2*time.Minute)); !slices.Contains(ids, short.ID) {
		t.Fatal("abandoned create was not claimed after its dispatch window")
	}

	// Without an explicit window the reservation keeps the five-minute gate.
	fallback := f.reserve(t, time.Time{})
	if ids := claimedIDs(t, f, now.Add(time.Minute)); slices.Contains(ids, fallback.ID) {
		t.Fatal("fresh create was claimed before the default window")
	}
	if ids := claimedIDs(t, f, now.Add(6*time.Minute)); !slices.Contains(ids, fallback.ID) {
		t.Fatal("abandoned create was not claimed after the default window")
	}
}

// Lifecycle drift under the worker's own claim is a handoff that releases the
// lease instead of stranding the row until the lease expires.
func TestIntegrationLifecycleDriftUnderOwnClaimReleasesLease(t *testing.T) {
	f := seedClaimFixture(t)
	ctx := t.Context()
	old := time.Now().Add(-10 * time.Minute)
	job := f.insertJob(t, claimJobSeed{Lifecycle: LifecycleCreating, State: StateQueued, UpdatedAt: &old})
	record := f.claim(t, job.ID)
	// A concurrent attach moves the row out of 'creating' while the claim is held.
	if _, err := AttachUpstream(ctx, f.pool, job.ID, "upstream-video-drift",
		JobUpdate{State: StateQueued, LastPolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before := f.gaps.Load()
	if outcome := f.service.reconcileClaimed(ctx, record); outcome != outcomeHandedOff {
		t.Fatalf("drift outcome %d", outcome)
	}
	if got := f.gaps.Load(); got != before {
		t.Fatalf("drift counted as a gap: %d", got)
	}
	if claimID, until := f.leaseOf(t, job.ID); claimID != nil || until != nil {
		t.Fatalf("claim leaked: %v %v", claimID, until)
	}
	if _, ok, err := ClaimJob(ctx, f.pool, job.ID, time.Now()); err != nil || !ok {
		t.Fatalf("job still busy: ok=%v err=%v", ok, err)
	}
}

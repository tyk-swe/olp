package routing

import (
	"testing"
	"time"
)

func TestCommitmentCoverageAtBoundary(t *testing.T) {
	boundary := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := Provider{Enabled: false, Commitment: &Commitment{ID: "old", Amount: "10", Period: "day", EndsAt: &boundary}, PendingCommitment: &Commitment{ID: "next", Amount: "300", Period: "month", EffectiveAt: &boundary}}
	if c := p.CommitmentAt(boundary.Add(-time.Nanosecond)); c == nil || c.ID != "old" {
		t.Fatalf("before boundary: %+v", c)
	}
	if c := p.CommitmentAt(boundary); c == nil || c.ID != "next" {
		t.Fatalf("at boundary: %+v", c)
	}
	p.PendingCommitment = nil
	if c := p.CommitmentAt(boundary); c != nil {
		t.Fatalf("removed commitment still covers: %+v", c)
	}
}

func TestCommitmentTimelineIsOwnedAndSigned(t *testing.T) {
	at := time.Now().UTC()
	original := &Snapshot{Commitments: map[string][]Commitment{"removed": {{ID: "historical", Amount: "7", Period: "hour", EffectiveAt: &at}}}}
	cloned := original.Clone()
	before, err := cloned.Digest()
	if err != nil {
		t.Fatal(err)
	}
	want := at
	original.Commitments["removed"][0].Amount = "8"
	*original.Commitments["removed"][0].EffectiveAt = at.Add(time.Hour)
	if cloned.Commitments["removed"][0].Amount != "7" || !cloned.Commitments["removed"][0].EffectiveAt.Equal(want) {
		t.Fatal("clone borrowed mutable timeline")
	}
	after, err := original.Digest()
	if err != nil || before == after {
		t.Fatal("commitment timeline is not covered by release digest", err)
	}
}

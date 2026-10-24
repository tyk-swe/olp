package resources

import (
	"github.com/tyk-swe/olp/internal/usage"
	"testing"
	"time"
)

func TestRetainedEvidenceExactNormalization(t *testing.T) {
	start := time.Date(2026, 10, 23, 1, 2, 3, 123456789, time.UTC)
	line := RetainedEvidenceLine{Component: "storage", Model: "m", Start: start, End: start.Add(time.Second), Usage: usage.AttemptUsage{StorageByteSeconds: new("0018446744073709551616000000000.1234567890")}}
	normalized, err := NormalizeRetainedEvidence(line)
	if err != nil {
		t.Fatal(err)
	}
	if *normalized.Usage.StorageByteSeconds != "18446744073709551616000000000.123456789" || normalized.Start.Nanosecond() != 123456000 || !normalized.Usage.Deferred {
		t.Fatalf("bad normalization: %+v", normalized)
	}
	if sameJSON([]byte(`{"training_tokens":9007199254740992}`), []byte(`{"training_tokens":9007199254740993}`)) {
		t.Fatal("large exact quantities compared equal")
	}
	if !sameJSON([]byte(`{"b":2,"a":1}`), []byte(`{"a":1,"b":2}`)) {
		t.Fatal("object ordering changed evidence identity")
	}
}
func TestRetainedEvidenceRefusesOmittedAndUnrelatedQuantities(t *testing.T) {
	now := time.Now().UTC()
	base := RetainedEvidenceLine{Component: "training", Model: "m", Start: now, End: now.Add(time.Second), Usage: usage.AttemptUsage{TrainingTokens: new(int64(0))}}
	if _, err := NormalizeRetainedEvidence(base); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RetainedEvidenceLine){
		func(l *RetainedEvidenceLine) { l.Usage.TrainingTokens = nil },
		func(l *RetainedEvidenceLine) { l.Usage.MediaUnits = new("1") },
		func(l *RetainedEvidenceLine) { l.Usage.Batch = true },
		func(l *RetainedEvidenceLine) { l.Start = l.Start.In(time.FixedZone("offset", 3600)) },
		func(l *RetainedEvidenceLine) { l.End = l.Start.Add(time.Nanosecond) },
		func(l *RetainedEvidenceLine) {
			l.Component = "generation"
			l.Usage = usage.AttemptUsage{InputTokens: new(int64(1)), OutputTokens: new(int64(1))}
		},
	} {
		line := base
		mutate(&line)
		if _, err := NormalizeRetainedEvidence(line); err == nil {
			t.Fatalf("invalid evidence accepted: %+v", line)
		}
	}
}

func TestRetainedAutomaticEvidenceDeduplicatesUnchangedPolls(t *testing.T) {
	first := RetainedObservation{RemoteTerminated: true, ObservedAt: time.Now().UTC()}
	later := first
	later.ObservedAt = later.ObservedAt.Add(time.Hour)
	if retainedEvidenceKey(first) != retainedEvidenceKey(later) || retainedObservationDigest(first) != retainedObservationDigest(later) {
		t.Fatal("unchanged polling created distinct financial evidence")
	}
	changed := later
	changed.CoverageComplete = true
	if retainedEvidenceKey(first) == retainedEvidenceKey(changed) {
		t.Fatal("changed coverage proof collapsed into an old observation")
	}
}

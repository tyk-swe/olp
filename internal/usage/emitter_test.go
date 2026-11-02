package usage

import (
	"testing"
	"time"
)

func TestEmitterLossTimestampsFollowItsClock(t *testing.T) {
	current := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	emitter := NewEmitter(1)
	emitter.now = func() time.Time { return current }
	emitter.Drop()
	snapshot := emitter.Snapshot()
	if snapshot.Dropped != 1 || snapshot.FirstLossAt == nil || !snapshot.FirstLossAt.Equal(current) ||
		snapshot.LastLossAt == nil || !snapshot.LastLossAt.Equal(current) {
		t.Fatalf("first loss snapshot = %+v, want one drop stamped at %v", snapshot, current)
	}
	current = current.Add(5 * time.Second)
	emitter.Drop()
	snapshot = emitter.Snapshot()
	if snapshot.Dropped != 2 || snapshot.FirstLossAt == nil || !snapshot.FirstLossAt.Equal(current.Add(-5*time.Second)) ||
		snapshot.LastLossAt == nil || !snapshot.LastLossAt.Equal(current) {
		t.Fatalf("second loss snapshot = %+v, want the last stamp advanced to %v", snapshot, current)
	}
}

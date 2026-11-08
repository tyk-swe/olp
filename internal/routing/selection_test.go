package routing

import (
	"bytes"
	"math"
	"runtime"
	"testing"

	"github.com/google/uuid"
)

func TestScoreClampsNonPositiveWeight(t *testing.T) {
	routeID := uuid.New()
	targetID := uuid.New()
	floor := Score(routeID, targetID, 1, "generation", "openai", "unary", []byte("affinity"))
	for _, weight := range []int64{0, -3} {
		if got := Score(routeID, targetID, weight, "generation", "openai", "unary", []byte("affinity")); got != floor {
			t.Fatalf("Score(weight %d) = %v, want floor %v", weight, got, floor)
		}
	}
	if got := Score(routeID, targetID, 2, "generation", "openai", "unary", []byte("affinity")); got != 2*floor {
		t.Fatalf("Score(weight 2) = %v, want %v", got, 2*floor)
	}
}

func TestScorePreservesAffinityAndBoundsAllocations(t *testing.T) {
	routeID := uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff")
	targetID := uuid.MustParse("ffeeddcc-bbaa-9988-7766-554433221100")
	longAffinity := bytes.Repeat([]byte("x"), 2<<20)
	for _, tc := range []struct {
		affinity []byte
		want     float64
	}{
		{nil, 0.6257423235384676},
		{longAffinity[:128], 4.227616064095782},
		{longAffinity, 3.8856422330125624},
	} {
		got := Score(routeID, targetID, 1, "generation", "openai", "unary", tc.affinity)
		if math.Abs(got-tc.want) > 1e-12*tc.want {
			t.Fatalf("score for %d-byte affinity = %v, want %v", len(tc.affinity), got, tc.want)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 8 {
		Score(routeID, targetID, 1, "generation", "openai", "unary", longAffinity)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("eight scores allocated %d bytes for a %d-byte affinity", allocated, len(longAffinity))
	}
}

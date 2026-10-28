package routing

import (
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

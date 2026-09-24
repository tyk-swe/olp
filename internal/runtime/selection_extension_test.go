package runtime

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestScoreRetainsLegacyVectorAndSeparatesRegisteredDimensions(t *testing.T) {
	route := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	target := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	affinity := []byte("sticky-user")
	// Captured from the original weighted-rendezvous input and Go 1.27.1
	// before registered operation names were added to the hash.
	legacy := Score(route, target, 3, "generation", "openai", "unary", affinity)
	if bits := math.Float64bits(legacy); bits != 4611584345176207231 {
		t.Fatalf("published legacy affinity score changed: %d", bits)
	}
	// The original fallback operation and surface labels also keep their
	// published ordering, including Bedrock and realtime.
	if Score(route, target, 3, "rerank", "bedrock", "realtime", affinity) != Score(route, target, 3, "batch", "openai", "unary", affinity) {
		t.Fatal("an existing fallback affinity score changed")
	}
	inputs := [][3]string{
		{"classification", "native", "unary"},
		{"scoring", "native", "unary"},
		{"classification", "openai", "unary"},
		{"classification", "other-native", "unary"},
		{"new_ab", "c", "unary"},
		{"new_a", "bc", "unary"},
	}
	seen := map[uint64]bool{}
	for _, input := range inputs {
		score := Score(route, target, 3, input[0], input[1], input[2], affinity)
		bits := math.Float64bits(score)
		if seen[bits] || math.Float64bits(Score(route, target, 3, input[0], input[1], input[2], affinity)) != bits {
			t.Fatalf("registered routing dimensions collided or changed: %v", input)
		}
		seen[bits] = true
	}
}

package runtime

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestScoreSeparatesRoutingDimensions(t *testing.T) {
	route := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	target := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	inputs := [][4]string{
		{"generation", "openai", "unary", "sticky-user"},
		{"generation", "openai", "streaming", "sticky-user"},
		{"generation", "anthropic", "unary", "sticky-user"},
		{"embeddings", "openai", "unary", "sticky-user"},
		{"generation", "openai", "unary", "other-user"},
		{"classification", "native", "unary", ""},
		{"new_ab", "c", "unary", ""},
		{"new_a", "bc", "unary", ""},
		{"generation", "openai", "unaryx", ""},
		{"generation", "openai", "unary", "x"},
	}
	seen := map[uint64][4]string{}
	for _, input := range inputs {
		score := Score(route, target, 3, input[0], input[1], input[2], []byte(input[3]))
		bits := math.Float64bits(score)
		if previous, collided := seen[bits]; collided {
			t.Fatalf("routing dimensions %v and %v share a score", previous, input)
		}
		if again := Score(route, target, 3, input[0], input[1], input[2], []byte(input[3])); math.Float64bits(again) != bits {
			t.Fatalf("score is not deterministic for %v", input)
		}
		seen[bits] = input
	}
}

func TestScoreDistributesFirstChoiceByWeight(t *testing.T) {
	route := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	targets := []struct {
		id     uuid.UUID
		weight int64
		share  float64
	}{
		{uuid.MustParse("00000000-0000-0000-0000-000000000011"), 1, 0.1},
		{uuid.MustParse("00000000-0000-0000-0000-000000000012"), 3, 0.3},
		{uuid.MustParse("00000000-0000-0000-0000-000000000013"), 6, 0.6},
	}
	const samples = 30000
	wins := make([]int, len(targets))
	for i := range samples {
		affinity := []byte(fmt.Sprintf("tenant-%d", i))
		best, bestScore := 0, 0.0
		for j, target := range targets {
			if score := Score(route, target.id, target.weight, "generation", "openai", "unary", affinity); score > bestScore {
				best, bestScore = j, score
			}
		}
		wins[best]++
	}
	for j, target := range targets {
		if share := float64(wins[j]) / samples; math.Abs(share-target.share) > 0.015 {
			t.Errorf("weight %d won %.3f of first choices, want %.2f", target.weight, share, target.share)
		}
	}
}

func TestScoreTreatsNonPositiveWeightAsOne(t *testing.T) {
	route := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	target := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	one := Score(route, target, 1, "generation", "openai", "unary", []byte("a"))
	for _, weight := range []int64{0, -5} {
		if Score(route, target, weight, "generation", "openai", "unary", []byte("a")) != one {
			t.Fatalf("weight %d did not score as weight 1", weight)
		}
	}
}

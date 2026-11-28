package estimate

import (
	"math"
	"strings"
	"testing"
)

func TestFactorsScaleOnlyFamiliesWithoutATokenizer(t *testing.T) {
	text := strings.Repeat("scaled estimate ", 64)
	factors := map[Family]float64{FamilyAnthropic: 1.5, FamilyOpenAIO200k: 1.5}
	scaled, provenance := ForModel("claude-sonnet", factors).Count(text)
	if want := int64(math.Ceil(float64(HeuristicTokens(text)) * 1.5)); scaled != want || provenance != ProvenanceCalibrated {
		t.Fatalf("anthropic estimate = %d, %s, want %d, %s", scaled, provenance, want, ProvenanceCalibrated)
	}
	if plain, provenance := ForModel("gemini-pro", factors).Count(text); plain != HeuristicTokens(text) || provenance != ProvenanceHeuristic {
		t.Fatalf("family without a factor = %d, %s", plain, provenance)
	}
	exact, _ := ForModel("gpt-4o", nil).Count(text)
	if counted, provenance := ForModel("gpt-4o", factors).Count(text); counted != exact || provenance != ProvenanceTokenizer {
		t.Fatalf("tokenizer count = %d, %s, want %d, %s", counted, provenance, exact, ProvenanceTokenizer)
	}
}

// TestAMeterReadsNoMoreThanItsWindow holds a Meter to the contract a walked
// prompt relies on to keep only part of its text: given the bytes of a segment
// up to the window past the budget, and how many there are besides, it counts
// the segment as it counts the whole of it. The texts put a piece boundary at
// every place a cut can fall: in prose, inside a multi-byte character, in a run
// of spaces after a line break, and in the middle of one long run.
func TestAMeterReadsNoMoreThanItsWindow(t *testing.T) {
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 3*ExactBytes/45)
	for name, text := range map[string]string{
		"prose":                               prose,
		"cjk":                                 strings.Repeat("\U00004e2d\U000065e5\U0000672c", ExactBytes),
		"emoji":                               strings.Repeat("ab\U0001f600", ExactBytes),
		"after a line break, a run of spaces": prose[:ExactBytes-40] + "\n" + strings.Repeat(" ", 3*ExactBytes) + "end",
		"one long run":                        prose[:ExactBytes-40] + strings.Repeat("z", 3*ExactBytes),
		"punctuation":                         strings.Repeat("]);\n", ExactBytes),
	} {
		for _, model := range []string{"gpt-4o", "gpt-4"} {
			c := ForModel(model, nil)
			for _, lead := range []int{0, 1, 2, 3, 100, ExactBytes - 7, ExactBytes - 1, ExactBytes} {
				if lead > len(text) {
					continue
				}
				whole := c.Meter()
				whole.add(text[:lead], 0, -1)
				whole.Add(text[lead:])

				// A prompt that kept the segment's first bytes up to the window
				// beyond whatever the lead left of the budget.
				kept := c.Meter()
				kept.add(text[:lead], 0, -1)
				rest := text[lead:]
				keep := prefixOf(rest, max(ExactBytes-lead, 0)+2*scanWindow)
				kept.add(keep, int64(len(rest)-len(keep)), HeuristicTokens(rest))

				if whole.tokens != kept.tokens || whole.exactBytes != kept.exactBytes || whole.tailBytes != kept.tailBytes || whole.encoding != kept.encoding {
					t.Errorf("%s on %s after %d bytes: the whole segment is %d tokens over %d exact bytes with a %d-byte tail, its window %d over %d with %d",
						name, model, lead, whole.tokens, whole.exactBytes, whole.tailBytes, kept.tokens, kept.exactBytes, kept.tailBytes)
				}
				a, pa := whole.Total()
				if b, pb := kept.Total(); a != b || pa != pb {
					t.Errorf("%s on %s after %d bytes: totals %d, %s and %d, %s", name, model, lead, a, pa, b, pb)
				}
			}
		}
	}
}

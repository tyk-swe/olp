package estimate

import (
	"strings"
	"testing"
)

// The gateway's admission estimate has charged these since before this
// package: characters, not bytes, four to a token, rounded up.

// four characters of twelve bytes

// Two, none, one and three: every segment is rounded up on its own, as
// the gateway has always done, so no field is free.

// unset

// 10.5 rounded up

// The exact part is a whole number of pieces within the bound, and the
// extrapolation is arithmetic on it.

// about a kilobyte

// One sentence repeated has the same ratio throughout, so the estimate is
// exact here, up to rounding up.

// What the budget has left is a few bytes, and "hi" would fit in it. It
// comes after text that did not, so it is part of the tail all the same: the
// ratio applies to one contiguous tail.

// One unbroken piece longer than the whole budget: nothing is counted
// exactly, so there is no ratio to scale the rest by, and the meter is a
// heuristic one, by characters like every heuristic count.

// Later text joins the heuristic count, and nothing is counted twice.

// A head too small to say anything, of any ratio: the run costs what the
// four-bytes rule says, whatever the head cost per byte.

// A head that is a real sample is a ratio, and the result says so.

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
			c := ForModel(model)
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

// "<|endoftext|>" is the three pieces "<|", "endoftext" and "|>", never the
// single end-of-text token, 199999 in o200k_base and 100257 in cl100k_base.

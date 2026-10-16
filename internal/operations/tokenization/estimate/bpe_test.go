package estimate

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// syntheticVocab builds a vocabulary of the 256 byte tokens followed by extra,
// in rank order, so a test can state merge priorities directly.
func syntheticVocab(t testing.TB, extra ...string) *vocab {
	t.Helper()
	lengths, blob := byteTokens()
	for _, token := range extra {
		lengths = appendUvarint(lengths, uint64(len(token)))
		blob = append(blob, token...)
	}
	data := append(append(appendUvarint([]byte(rankMagic), uint64(256+len(extra))), lengths...), blob...)
	v, err := newVocab(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// referenceBounds is the textbook merge with no size limit and no cleverness:
// find the leftmost pair whose concatenation has the lowest rank, merge it, and
// repeat. It returns the part boundaries. tiktoken's own merge matches it on
// every fixture, and the heap and the scan are checked against it.
func referenceBounds(v *vocab, p string) []int {
	bounds := make([]int, len(p)+1)
	for i := range bounds {
		bounds[i] = i
	}
	for {
		best, at := uint32(noRank), -1
		for i := 0; i+2 < len(bounds); i++ {
			if r := v.rank(p[bounds[i]:bounds[i+2]]); r < best {
				best, at = r, i
			}
		}
		if at < 0 {
			return bounds
		}
		bounds = slices.Delete(bounds, at+1, at+2)
	}
}

// smallBounds and largeBounds run the two production merges and return their
// boundaries in the reference's form.
func smallBounds(v *vocab, p string) []int {
	var b [smallPiece + 1]uint8
	n := v.mergeSmall(p, &b)
	out := make([]int, n+1)
	for i := range out {
		out[i] = int(b[i])
	}
	return out
}

func largeBounds(v *vocab, p string) []int {
	sc := getScratch(len(p))
	defer sc.release()
	n := v.mergeLarge(p, sc)
	out := []int{0}
	for i := int32(0); int(sc.next[i]) < len(p); i = sc.next[i] {
		out = append(out, int(sc.next[i]))
	}
	out = append(out, len(p))
	if len(out)-1 != n {
		panic("mergeLarge reported a different part count than it linked")
	}
	return out
}

// Equal pairs overlap, and the leftmost merges first: "aaa" is
// "aa" and "a", never "a" and "aa".

// A lower rank wins over a position further left.

// Merges build on earlier merges.

// The scan serves short pieces and the heap long ones, so both must
// agree here, and with the reference.

// randomPiece builds text that merges a lot: real tokens strung together, with
// stray bytes and repeats, so there are ties, chains and dead ends.
func randomPiece(rng *rand.Rand, v *vocab, n int) string {
	var b strings.Builder
	for b.Len() < n {
		switch rng.IntN(8) {
		case 0:
			b.WriteByte(byte(rng.IntN(256)))
		case 1:
			b.WriteString(strings.Repeat(string(rune('a'+rng.IntN(3))), 1+rng.IntN(9)))
		default:
			b.WriteString(v.token(uint32(rng.IntN(len(v.offsets) - 1))))
		}
	}
	return b.String()[:n]
}

// around the switch between the two

// Long pieces only the heap serves. The reference is quadratic, so
// these are a few kilobytes, and fewer under the race detector.

// Arbitrary bytes, valid UTF-8 or not.

// fastestPair is the shortest of several runs of Count over each of two texts,
// run alternately so that a spike of load on a shared machine lands on both of
// them and not on one, and leaves out the time it took from either.
func fastestPair(t *testing.T, e *Encoding, small, large string) (time.Duration, time.Duration) {
	t.Helper()
	fastest := func(text string, best *time.Duration) {
		start := time.Now()
		if _, err := e.Count(text); err != nil {
			t.Fatal(err)
		}
		*best = min(*best, time.Since(start))
	}
	bestSmall, bestLarge := time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	for range 5 {
		fastest(small, &bestSmall)
		fastest(large, &bestLarge)
	}
	return bestSmall, bestLarge
}

// The padding moves the cut through every byte of the rune.

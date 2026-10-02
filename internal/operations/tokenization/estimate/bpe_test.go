package estimate

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestMergeBreaksTiesLeftmost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		piece string
		want  []string
	}{
		// Equal pairs overlap, and the leftmost merges first: "aaa" is
		// "aa" and "a", never "a" and "aa".
		{"overlapping equal pairs", []string{"aa"}, "aaa", []string{"aa", "a"}},
		{"an even run", []string{"aa"}, "aaaa", []string{"aa", "aa"}},
		{"an odd run", []string{"aa"}, "aaaaa", []string{"aa", "aa", "a"}},
		// A lower rank wins over a position further left.
		{"lower rank first", []string{"bc", "ab"}, "abc", []string{"a", "bc"}},
		{"other order", []string{"ab", "bc"}, "abc", []string{"ab", "c"}},
		// Merges build on earlier merges.
		{"merged parts merge again", []string{"ab", "abc", "abcd"}, "abcd", []string{"abcd"}},
		{"a merge enables the one before it", []string{"bc", "abc"}, "abc", []string{"abc"}},
		{"a token that needs a path through other tokens", []string{"cd", "abcd"}, "abcd", []string{"a", "b", "cd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := syntheticVocab(t, tc.extra...)
			// The scan serves short pieces and the heap long ones, so both must
			// agree here, and with the reference.
			for name, bounds := range map[string][]int{
				"scan":      smallBounds(v, tc.piece),
				"heap":      largeBounds(v, tc.piece),
				"reference": referenceBounds(v, tc.piece),
			} {
				var got []string
				for i := 0; i+1 < len(bounds); i++ {
					got = append(got, tc.piece[bounds[i]:bounds[i+1]])
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("%s merges %q into %q, want %q", name, tc.piece, got, tc.want)
				}
			}
		})
	}
}

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

func TestMergesAgreeWithReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		if err := e.load(); err != nil {
			t.Fatal(err)
		}
		v := e.vocab
		t.Run(e.name, func(t *testing.T) {
			n := 3_000
			if testing.Short() {
				n = 300
			}
			for range n {
				size := 2 + rng.IntN(60)
				if rng.IntN(5) == 0 {
					size = smallPiece - 2 + rng.IntN(5) // around the switch between the two
				}
				piece := randomPiece(rng, v, size)
				want := referenceBounds(v, piece)
				if size <= smallPiece {
					if got := smallBounds(v, piece); !slices.Equal(got, want) {
						t.Fatalf("the scan merges %q into bounds %v, the reference into %v", piece, got, want)
					}
				}
				if got := largeBounds(v, piece); !slices.Equal(got, want) {
					t.Fatalf("the heap merges %q into bounds %v, the reference into %v", piece, got, want)
				}
			}
			// Long pieces only the heap serves. The reference is quadratic, so
			// these are a few kilobytes.
			for range 12 {
				piece := randomPiece(rng, v, 1_000+rng.IntN(3_000))
				if got, want := largeBounds(v, piece), referenceBounds(v, piece); !slices.Equal(got, want) {
					t.Fatalf("the heap disagrees with the reference on a %d-byte piece", len(piece))
				}
			}
		})
	}
}

// TestScratchReuseDoesNotLeakState runs merges of shrinking and growing sizes
// through the pool, so a scratch left dirty by a longer piece would show.
func TestScratchReuseDoesNotLeakState(t *testing.T) {
	e := CL100kBase
	if err := e.load(); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 8))
	for _, size := range []int{900, 40, 300, 5, 2_000, 33, 129, 700} {
		piece := randomPiece(rng, e.vocab, size)
		if got, want := largeBounds(e.vocab, piece), referenceBounds(e.vocab, piece); !slices.Equal(got, want) {
			t.Fatalf("size %d: the heap disagrees with the reference after reuse", size)
		}
	}
}

// TestTokensConcatenateToInput checks the encoder from the other end: whatever
// the merges decide, the tokens' bytes must spell the text.
func TestTokensConcatenateToInput(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		t.Run(e.name, func(t *testing.T) {
			if err := e.load(); err != nil {
				t.Fatal(err)
			}
			check := func(text string) {
				ids, err := e.Encode(text)
				if err != nil {
					t.Fatal(err)
				}
				var spelled strings.Builder
				for _, id := range ids {
					spelled.WriteString(e.vocab.token(id))
				}
				if spelled.String() != text {
					t.Fatalf("the tokens of %.60q spell %.60q", text, spelled.String())
				}
				if n, err := e.Count(text); err != nil || n != len(ids) {
					t.Fatalf("Count(%.60q) = %d, %v, but Encode made %d tokens", text, n, err, len(ids))
				}
			}
			for _, f := range loadFixtures(t, "o200k_base.json") {
				check(f.Text)
			}
			for range 2_000 {
				check(randomPiece(rng, e.vocab, rng.IntN(300)))
			}
			// Arbitrary bytes, valid UTF-8 or not.
			for range 500 {
				raw := make([]byte, rng.IntN(200))
				for i := range raw {
					raw[i] = byte(rng.IntN(256))
				}
				check(string(raw))
			}
		})
	}
}

// fastest is the shortest of several runs of Count over text, which leaves out
// the time a busy machine took from the others.
func fastest(t *testing.T, e *Encoding, text string) time.Duration {
	t.Helper()
	best := time.Duration(math.MaxInt64)
	for range 5 {
		start := time.Now()
		if _, err := e.Count(text); err != nil {
			t.Fatal(err)
		}
		best = min(best, time.Since(start))
	}
	return best
}

// TestLongRunsAreNotQuadratic is the reason for the heap: one unbroken piece
// must take time in proportion to its size, which a pass per merge would not.
// How long a count takes says nothing across machines, or under the race
// detector, so a piece is timed against one an eighth of its size. An eighth of
// the size is an eighth of the time, a little more for the heap, and sixty four
// times the time for a pass per merge: the limit is four times linear. The
// guard runs under -short too. The full run also counts the largest piece the
// merge takes at once, which only has to finish.
func TestLongRunsAreNotQuadratic(t *testing.T) {
	const size, ratio = 1 << 16, 8
	for name, build := range map[string]func(n int) string{
		"letters": func(n int) string { return strings.Repeat("a", n) },
		"spaces":  func(n int) string { return strings.Repeat(" ", n) },
		"symbols": func(n int) string { return strings.Repeat("=", n) },
		"cjk":     func(n int) string { return strings.Repeat("\U00004e2d\U00006587", n/6) },
	} {
		for _, e := range []*Encoding{O200kBase, CL100kBase} {
			t.Run(name+"/"+e.Name(), func(t *testing.T) {
				if _, err := e.Count("warm the encoding up"); err != nil {
					t.Fatal(err)
				}
				small, large := fastest(t, e, build(size/ratio)), fastest(t, e, build(size))
				if large > 4*ratio*small {
					t.Errorf("%d bytes took %v and %d bytes took %v: %.0f times as long for %d times the text",
						size/ratio, small, size, large, float64(large)/float64(small), ratio)
				}
				if testing.Short() {
					return
				}
				if _, err := e.Count(build(maxPiece - 1)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// TestPiecesPastTheMergeLimitAreChunked covers the one place counting is only
// approximate: a piece longer than maxPiece is merged in chunks, so its count
// is that of the chunks.
func TestPiecesPastTheMergeLimitAreChunked(t *testing.T) {
	if testing.Short() {
		t.Skip("encodes pieces of a quarter megabyte")
	}
	e := CL100kBase
	if err := e.load(); err != nil {
		t.Fatal(err)
	}
	piece := strings.Repeat("a", maxPiece+5)
	want := e.vocab.countLarge(piece[:maxPiece]) + e.vocab.countLarge(piece[maxPiece:])
	if got := e.vocab.countPiece(piece); got != want {
		t.Fatalf("a %d-byte piece counts %d tokens, want the chunks' %d", len(piece), got, want)
	}
	ids := e.vocab.appendPiece(nil, piece)
	if len(ids) != want {
		t.Fatalf("a %d-byte piece encodes to %d tokens, want the chunks' %d", len(piece), len(ids), want)
	}
}

// TestChunksEndOnRuneBoundaries keeps the cut of a piece past the limit from
// splitting a rune, which would count the halves as stray bytes. Whatever the
// piece is, the cut is within a rune's length of the limit, and a piece that
// has no rune boundary there is cut at the limit and still terminates.
func TestChunksEndOnRuneBoundaries(t *testing.T) {
	for _, r := range []string{"\u00e9", "\u4e2d", "\U0001f600"} {
		// The padding moves the cut through every byte of the rune.
		for pad := range len(r) {
			p := strings.Repeat("a", pad) + strings.Repeat(r, maxPiece/len(r)+1)
			n := chunkLen(p)
			if n > maxPiece || n <= maxPiece-utf8.UTFMax || !utf8.RuneStart(p[n]) || !utf8.ValidString(p[:n]) {
				t.Errorf("%q after %d bytes of padding is cut at %d of %d", r, pad, n, maxPiece)
			}
		}
	}

	junk := strings.Repeat("\x80", maxPiece+5)
	if n := chunkLen(junk); n != maxPiece {
		t.Errorf("a piece of continuation bytes is cut at %d, want %d", n, maxPiece)
	}
	e := CL100kBase
	if err := e.load(); err != nil {
		t.Fatal(err)
	}
	if got, want := e.vocab.countPiece(junk), e.vocab.countLarge(junk[:maxPiece])+e.vocab.countLarge(junk[maxPiece:]); got != want {
		t.Errorf("a piece of continuation bytes counts %d tokens, want the chunks' %d", got, want)
	}
}

// TestLongTextOfOneScriptCountsWholeRunes holds a piece past the limit to the
// count of tiktoken, which is a token for each character, where a cut inside a
// rune made two extra: tiktoken counts 100,000 tokens for it in both encodings.
func TestLongTextOfOneScriptCountsWholeRunes(t *testing.T) {
	const runes = 100_000
	text := strings.Repeat("\u4e2d", runes)
	if len(text) <= maxPiece {
		t.Fatalf("%d bytes do not pass the merge limit", len(text))
	}
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		t.Run(e.Name(), func(t *testing.T) {
			if one, err := e.Count("\u4e2d"); err != nil || one != 1 {
				t.Fatalf("one character counts %d tokens, %v; the case proves nothing", one, err)
			}
			if two, err := e.Count("\u4e2d\u4e2d"); err != nil || two != 2 {
				t.Fatalf("two characters count %d tokens, %v; the case proves nothing", two, err)
			}
			n, err := e.Count(text)
			if err != nil || n != runes {
				t.Fatalf("%d characters count %d tokens, %v, want %d", runes, n, err, runes)
			}
			ids, err := e.Encode(text)
			if err != nil || len(ids) != runes {
				t.Fatalf("%d characters encode to %d tokens, %v, want %d", runes, len(ids), err, runes)
			}
		})
	}
}

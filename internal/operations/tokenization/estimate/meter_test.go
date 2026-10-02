package estimate

import (
	"encoding/base64"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestHeuristicMatchesTheGatewayRule(t *testing.T) {
	// The gateway's admission estimate has charged these since before this
	// package: characters, not bytes, four to a token, rounded up.
	for text, want := range map[string]int64{
		"": 0, "h": 1, "hi": 1, "hell": 1, "hello": 2, "hello wo": 2, "hello wor": 3,
		"\U000065e5\U0000672c\U00008a9e\U00007684":           1, // four characters of twelve bytes
		"\U000065e5\U0000672c\U00008a9e\U00007684\U000065e5": 2,
	} {
		if got := HeuristicTokens(text); got != want {
			t.Errorf("HeuristicTokens(%q) = %d, want %d", text, got, want)
		}
	}
	for n, want := range map[int]int64{0: 0, 1: 1, 4: 1, 5: 2, 8: 2, 9: 3} {
		if got := HeuristicBytesTokens(n); got != want {
			t.Errorf("HeuristicBytesTokens(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestHeuristicMeterChargesEachSegment(t *testing.T) {
	m := ForModel("claude-sonnet-4-5").Meter()
	for _, segment := range []string{"hello", "", "hi", "hello wor"} {
		m.Add(segment)
	}
	// Two, none, one and three: every segment is rounded up on its own, as
	// the gateway has always done, so no field is free.
	if n, provenance := m.Total(); n != 6 || provenance != ProvenanceHeuristic {
		t.Fatalf("Total = %d, %s; want 6, heuristic", n, provenance)
	}
}

func TestFactorMakesTheHeuristicCalibrated(t *testing.T) {
	for _, tc := range []struct {
		factor float64
		want   int64
		prov   Provenance
	}{
		{0, 7, ProvenanceHeuristic}, // unset
		{1, 7, ProvenanceHeuristic},
		{-2, 7, ProvenanceHeuristic},
		{1.5, 11, ProvenanceCalibrated}, // 10.5 rounded up
		{0.5, 4, ProvenanceCalibrated},
		{2, 14, ProvenanceCalibrated},
	} {
		c := Counter{family: FamilyAnthropic, factor: tc.factor}
		n, provenance := c.Count(strings.Repeat("x", 25) + "yyy")
		if n != tc.want || provenance != tc.prov {
			t.Errorf("factor %v: %d, %s; want %d, %s", tc.factor, n, provenance, tc.want, tc.prov)
		}
	}
	if n, p := (Counter{family: FamilyGemini, factor: 1.25}).Count(""); n != 0 || p != ProvenanceCalibrated {
		t.Errorf("an empty text: %d, %s", n, p)
	}
}

func TestTokenizerCountsExactlyUnderTheLimit(t *testing.T) {
	for _, tc := range []struct{ file, model string }{
		{"o200k_base.json", "gpt-4o"},
		{"cl100k_base.json", "gpt-4"},
	} {
		c := ForModel(tc.model)
		for _, f := range loadFixtures(t, tc.file) {
			if len(f.Text) > ExactBytes {
				continue
			}
			n, provenance := c.Count(f.Text)
			if n != int64(len(f.Tokens)) || provenance != ProvenanceTokenizer {
				t.Errorf("%s/%s: %d tokens, %s; want %d, tokenizer", tc.model, f.Name, n, provenance, len(f.Tokens))
			}
		}
	}
}

func TestSegmentsDoNotMergeAcrossBoundaries(t *testing.T) {
	c := ForModel("gpt-4o")
	whole, _ := c.Count("hello world")
	m := c.Meter()
	m.Add("hello")
	m.Add(" world")
	apart, _ := m.Total()
	if whole != 2 || apart != 2 {
		t.Fatalf("hello world is %d tokens whole and %d in two segments, want 2 and 2", whole, apart)
	}
	m = c.Meter()
	m.Add("hello wor")
	m.Add("ld")
	if split, _ := m.Total(); split <= whole {
		t.Fatalf("a segment break inside a word should cost tokens, got %d against %d", split, whole)
	}
}

// longProse is English-like text well past ExactBytes, with exact counts to
// compare an estimate with.
func longProse(t *testing.T, e *Encoding, minBytes int) (text string, tokens int) {
	t.Helper()
	var doc string
	for _, f := range loadFixtures(t, "o200k_base.json") {
		if f.Name == "document-52k" {
			doc = f.Text
		}
	}
	if doc == "" {
		t.Fatal("no document-52k fixture")
	}
	text = strings.Repeat(doc+"\n", minBytes/len(doc)+1)
	tokens, err := e.Count(text)
	if err != nil {
		t.Fatal(err)
	}
	return text, tokens
}

func TestLongTextIsCountedExactlyThenExtrapolated(t *testing.T) {
	c := ForModel("gpt-4o")
	text, exact := longProse(t, O200kBase, 4*ExactBytes)
	got, provenance := c.Count(text)
	if provenance != ProvenanceCalibrated {
		t.Fatalf("provenance %s, want calibrated", provenance)
	}
	if err := math.Abs(float64(got-int64(exact))) / float64(exact); err > 0.02 {
		t.Errorf("extrapolated %d tokens for %d exact, %.1f%% off", got, exact, err*100)
	}

	// The exact part is a whole number of pieces within the bound, and the
	// extrapolation is arithmetic on it.
	m := c.Meter()
	m.Add(text)
	if m.exactBytes <= 0 || m.exactBytes > ExactBytes || m.exactBytes+m.tailBytes != int64(len(text)) {
		t.Fatalf("exact bytes %d, tail %d, of %d (bound %d)", m.exactBytes, m.tailBytes, len(text), ExactBytes)
	}
	prefix, err := O200kBase.Count(text[:m.exactBytes])
	if err != nil || int64(prefix) != m.tokens {
		t.Fatalf("the exact part counts %d tokens, not the %d the meter holds (%v)", prefix, m.tokens, err)
	}
	want := m.tokens + (m.tailBytes*m.tokens+m.exactBytes-1)/m.exactBytes
	if n, _ := m.Total(); n != want {
		t.Errorf("Total = %d, want exact %d plus the tail at the prefix's ratio, %d", n, m.tokens, want)
	}
}

func TestTheBoundCoversTheWholeRequest(t *testing.T) {
	c := ForModel("gpt-4o")
	segment := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 24) // about a kilobyte
	m := c.Meter()
	var all strings.Builder
	for range 100 {
		m.Add(segment)
		all.WriteString(segment)
	}
	if m.exactBytes > ExactBytes || m.exactBytes < ExactBytes-int64(len(segment)) {
		t.Errorf("%d bytes counted exactly of %d, want just under %d", m.exactBytes, all.Len(), ExactBytes)
	}
	n, provenance := m.Total()
	if provenance != ProvenanceCalibrated {
		t.Errorf("provenance %s, want calibrated", provenance)
	}
	// One sentence repeated has the same ratio throughout, so the estimate is
	// exact here, up to rounding up.
	exact := int64(0)
	for range 100 {
		one, _ := c.Count(segment)
		exact += one
	}
	if n < exact || n > exact+2 {
		t.Errorf("estimated %d tokens for %d exact", n, exact)
	}
}

func TestTextAfterTheTailJoinsTheTail(t *testing.T) {
	c := ForModel("gpt-4o")
	m := c.Meter()
	m.Add(strings.Repeat("word ", ExactBytes))
	exactBefore, tailBefore := m.exactBytes, m.tailBytes
	// What the budget has left is a few bytes, and "hi" would fit in it. It
	// comes after text that did not, so it is part of the tail all the same: the
	// ratio applies to one contiguous tail.
	if m.budget <= 0 || m.budget < len("hi") {
		t.Fatalf("the case needs a budget of at least two bytes left, has %d", m.budget)
	}
	m.Add("hi")
	m.Add("short")
	if m.exactBytes != exactBefore || m.tailBytes != tailBefore+int64(len("hi")+len("short")) {
		t.Fatalf("segments after the tail were counted exactly: %d exact bytes before, %d after; tail %d before, %d after",
			exactBefore, m.exactBytes, tailBefore, m.tailBytes)
	}
}

func TestAPieceLongerThanTheBoundHasNoExactPart(t *testing.T) {
	c := ForModel("gpt-4o")
	// One unbroken piece longer than the whole budget: nothing is counted
	// exactly, so there is no ratio to scale the rest by, and the meter is a
	// heuristic one, by characters like every heuristic count.
	for name, text := range map[string]string{
		"ascii": strings.Repeat("a", ExactBytes+100),
		"cjk":   strings.Repeat("\U00004e2d", ExactBytes/3+100),
	} {
		m := c.Meter()
		m.Add(text)
		if m.exactBytes != 0 || m.tokens != 0 || m.tailBytes != 0 {
			t.Fatalf("%s: counted %d bytes, %d tokens exactly, with a %d-byte tail", name, m.exactBytes, m.tokens, m.tailBytes)
		}
		want := HeuristicTokens(text)
		if n, provenance := m.Total(); n != want || provenance != ProvenanceHeuristic {
			t.Fatalf("%s: Total = %d, %s; want %d, heuristic", name, n, provenance, want)
		}
		// Later text joins the heuristic count, and nothing is counted twice.
		m.Add("hello")
		if n, provenance := m.Total(); n != want+2 || provenance != ProvenanceHeuristic {
			t.Fatalf("%s: after another segment Total = %d, %s; want %d, heuristic", name, n, provenance, want+2)
		}
	}
}

// TestATailIsExtrapolatedOnlyFromARealSample covers a piece that crosses the
// bound after some text was counted exactly. The ratio of that text prices the
// piece, so it is only as good as the text is long: a one-line prompt ahead of a
// long run of letters had the run priced by a ratio measured on a few bytes,
// which was twice the exact count for one prompt and under half of it for
// another.
func TestATailIsExtrapolatedOnlyFromARealSample(t *testing.T) {
	c := ForModel("gpt-4o")
	run := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 40_000/26+1)[:40_000]
	exactRun, err := O200kBase.Count(run)
	if err != nil {
		t.Fatal(err)
	}
	if HeuristicBytesTokens(len(run)) == int64(exactRun) {
		t.Fatalf("the run is %d tokens exactly and %d to the rule; the case proves nothing", exactRun, HeuristicBytesTokens(len(run)))
	}

	// A head too small to say anything, of any ratio: the run costs what the
	// four-bytes rule says, whatever the head cost per byte.
	for _, head := range []string{"a", "Hi", "You are a helpful assistant.", strings.Repeat("Hello, world! ", 100)} {
		if len(head) >= minSample {
			t.Fatalf("head %q is a sample", head[:8])
		}
		exactHead, err := O200kBase.Count(head)
		if err != nil {
			t.Fatal(err)
		}
		m := c.Meter()
		m.Add(head)
		m.Add(run)
		want := int64(exactHead) + HeuristicBytesTokens(len(run))
		if n, provenance := m.Total(); n != want || provenance != ProvenanceHeuristic {
			t.Errorf("a %d-byte head: Total = %d, %s; want %d, heuristic", len(head), n, provenance, want)
		}
	}

	// A head that is a real sample is a ratio, and the result says so.
	head := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 2*minSample/45)
	if len(head) < minSample || len(head) > ExactBytes {
		t.Fatalf("head of %d bytes is not a sample inside the bound", len(head))
	}
	m := c.Meter()
	m.Add(head)
	m.Add(run)
	if m.exactBytes != int64(len(head)) || m.tailBytes != int64(len(run)) {
		t.Fatalf("%d exact bytes and %d in the tail, want %d and %d", m.exactBytes, m.tailBytes, len(head), len(run))
	}
	if _, provenance := m.Total(); provenance != ProvenanceCalibrated {
		t.Errorf("a %d-byte sample gave a %s count, want calibrated", len(head), provenance)
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

// TestMixedPromptsStayWithinBounds holds the account of the approximation in
// ExactBytes to what it measures. A prompt of one kind of text is extrapolated
// almost exactly; a prompt that changes kind after the exact part is priced by
// the wrong ratio, and how wrong is bounded by what the two kinds cost per byte.
func TestMixedPromptsStayWithinBounds(t *testing.T) {
	prose := corpus(t, "document-52k")
	code := corpus(t, "code-go", "code-python", "code-javascript")
	structured := corpus(t, "tools-pretty", "json-pretty", "tools-compact")
	cjk := corpus(t, "multilingual-chinese", "multilingual-japanese", "multilingual-cjk-long")
	random := rand.New(rand.NewPCG(1, 2))
	bytes := make([]byte, 300_000)
	for i := range bytes {
		bytes[i] = byte(random.IntN(256))
	}
	encoded := base64.StdEncoding.EncodeToString(bytes)
	fill := func(unit string, n int) string { return prefixOf(strings.Repeat(unit+"\n", n/(len(unit)+1)+1), n) }
	for _, tc := range []struct {
		name                   string
		head, tail             string
		minPercent, maxPercent float64
	}{
		{"prose", fill(prose, 400_000), "", -0.5, 0.5},
		{"code", fill(code, 400_000), "", -0.5, 0.5},
		{"JSON", fill(structured, 400_000), "", -0.5, 0.5},
		{"Chinese and Japanese", fill(cjk, 400_000), "", -0.5, 0.5},
		{"prose, then JSON", fill(prose, 8_000), fill(structured, 392_000), -5, 5},
		{"prose, then code", fill(prose, 8_000), fill(code, 392_000), -10, 10},
		{"prose, then Chinese and Japanese", fill(prose, 8_000), fill(cjk, 392_000), -20, 5},
		{"prose, then base64", fill(prose, 8_000), encoded[:392_000], -20, 5},
		{"JSON, then prose", fill(structured, 8_000), fill(prose, 392_000), -5, 10},
		{"Chinese and Japanese, then prose", fill(cjk, 8_000), fill(prose, 392_000), -5, 25},
		{"a page of prose, then Chinese and Japanese", fill(prose, 30_000), fill(cjk, 370_000), -50, 5},
	} {
		for _, model := range []string{"gpt-4o", "gpt-4"} {
			c := ForModel(model)
			text := tc.head + tc.tail
			exact, err := c.encoding.Count(text)
			if err != nil {
				t.Fatal(err)
			}
			m := c.Meter()
			m.Add(text)
			got, provenance := m.Total()
			percent := 100 * float64(got-int64(exact)) / float64(exact)
			if provenance != ProvenanceCalibrated || percent < tc.minPercent || percent > tc.maxPercent {
				t.Errorf("%s on %s: %d tokens, %s, %+.1f%% against the exact %d; want calibrated within %+.1f%% and %+.1f%%",
					tc.name, model, got, provenance, percent, exact, tc.minPercent, tc.maxPercent)
			}
		}
	}
}

func TestEmptySegmentsCostNothing(t *testing.T) {
	for _, model := range []string{"gpt-4o", "claude-sonnet-4-5"} {
		m := ForModel(model).Meter()
		m.Add("")
		m.Add("")
		if n, _ := m.Total(); n != 0 {
			t.Errorf("%s: empty segments cost %d", model, n)
		}
	}
	var zero Meter
	zero.Add("hello")
	if n, p := zero.Total(); n != 2 || p != ProvenanceHeuristic {
		t.Errorf("the zero meter: %d, %s", n, p)
	}
}

// TestUnloadableEncodingFallsBackToTheHeuristic covers a corrupt binary: the
// request is estimated, honestly labeled, instead of failed.
func TestUnloadableEncodingFallsBackToTheHeuristic(t *testing.T) {
	c := Counter{family: FamilyOpenAIO200k, encoding: &Encoding{name: "broken", ranks: "not a rank file", scan: scanO200k}}
	if n, provenance := c.Count("hello"); n != 2 || provenance != ProvenanceHeuristic {
		t.Fatalf("Count = %d, %s; want 2, heuristic", n, provenance)
	}
	if _, err := c.encoding.Count("hello"); err == nil {
		t.Fatal("the broken encoding counted without error")
	}
	if _, err := c.encoding.Encode("hello"); err == nil {
		t.Fatal("the broken encoding encoded without error")
	}
}

func TestPreload(t *testing.T) {
	if err := Preload(); err != nil {
		t.Fatal(err)
	}
	if O200kBase.vocab == nil || CL100kBase.vocab == nil || O200kBase.classes == nil {
		t.Fatal("Preload left an encoding unloaded")
	}
}

func TestSpecialTokenTextIsOrdinaryText(t *testing.T) {
	// "<|endoftext|>" is the three pieces "<|", "endoftext" and "|>", never the
	// single end-of-text token, 199999 in o200k_base and 100257 in cl100k_base.
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		ids, err := e.Encode("<|endoftext|>")
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) < 3 {
			t.Errorf("%s encodes <|endoftext|> as %v", e.Name(), ids)
		}
		for _, id := range ids {
			if id >= 100257 && e == CL100kBase || id >= 199999 && e == O200kBase {
				t.Errorf("%s produced special token %d", e.Name(), id)
			}
		}
	}
}

func TestInvalidUTF8CountsEachByte(t *testing.T) {
	for _, e := range []*Encoding{O200kBase, CL100kBase} {
		got, err := e.Count("\xff\xfe\xfd")
		if err != nil || got != 3 {
			t.Errorf("%s counts three invalid bytes as %d, %v; want 3", e.Name(), got, err)
		}
	}
}

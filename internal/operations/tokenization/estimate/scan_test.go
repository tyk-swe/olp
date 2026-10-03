package estimate

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The reference splitter implements the two tiktoken patterns a second way,
// on top of Go's regexp, so a mistake in a scanner's reading of a pattern
// shows up as a disagreement instead of agreeing with itself. Go's regexp
// matches leftmost-first like the engine the patterns were written for, and it
// takes every alternative here but the lookahead, which is written by hand. It
// differs in two ways the patterns' text hides, and both are spelled out: \s is
// ASCII-only there, so whitespace is the Unicode White_Space list, and a
// possessive quantifier is a greedy one, which is equivalent in these patterns.

const whiteSpace = `\t-\r \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}`

const contractionSuffix = `(?i:'s|'t|'re|'ve|'m|'ll|'d)`

type referenceAlt struct {
	re *regexp.Regexp
	// match replaces re for the alternative regexp cannot express.
	match func(s string) int
}

func compileAlts(patterns ...string) []referenceAlt {
	var alts []referenceAlt
	for _, p := range patterns {
		if p == "" {
			alts = append(alts, referenceAlt{match: spaceNotFollowedByNonSpace})
			continue
		}
		alts = append(alts, referenceAlt{re: regexp.MustCompile(`^(?:` + strings.ReplaceAll(p, "WS", whiteSpace) + `)`)})
	}
	return alts
}

var (
	referenceCL100k = compileAlts(
		`'(?i:[sdmt]|ll|ve|re)`,
		`[^\r\n\p{L}\p{N}]?\p{L}+`,
		`\p{N}{1,3}`,
		` ?[^WS\p{L}\p{N}]+[\r\n]*`,
		`[WS]+\z`,
		`[WS]*[\r\n]`,
		``, // \s+(?!\S)
		`[WS]`,
	)
	referenceO200k = compileAlts(
		`[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+`+contractionSuffix+`?`,
		`[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*`+contractionSuffix+`?`,
		`\p{N}{1,3}`,
		` ?[^WS\p{L}\p{N}]+[\r\n/]*`,
		`[WS]*[\r\n]+`,
		``, // \s+(?!\S)
		`[WS]+`,
	)
)

func isWhiteSpace(r rune) bool {
	switch {
	case r >= 0x9 && r <= 0xD, r == ' ', r == 0x85, r == 0xA0, r == 0x1680, r >= 0x2000 && r <= 0x200A,
		r == 0x2028, r == 0x2029, r == 0x202F, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

// spaceNotFollowedByNonSpace is \s+(?!\S): the longest whitespace run that is
// followed by whitespace or the end of the text, backing off one rune at a
// time.
func spaceNotFollowedByNonSpace(s string) int {
	var ends []int
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		if !isWhiteSpace(r) {
			break
		}
		i += w
		ends = append(ends, i)
	}
	for k := len(ends) - 1; k >= 0; k-- {
		end := ends[k]
		if end == len(s) {
			return end
		}
		if r, _ := utf8.DecodeRuneInString(s[end:]); isWhiteSpace(r) {
			return end
		}
	}
	return -1
}

// referenceSplit splits s the way the engine would: at each position the first
// alternative that matches wins.
func referenceSplit(alts []referenceAlt, s string) []string {
	var pieces []string
	for len(s) > 0 {
		n := -1
		for _, alt := range alts {
			if alt.match != nil {
				n = alt.match(s)
			} else if loc := alt.re.FindStringIndex(s); loc != nil {
				n = loc[1]
			}
			if n > 0 {
				break
			}
		}
		if n <= 0 {
			panic(fmt.Sprintf("no alternative matches %+q", s))
		}
		pieces = append(pieces, s[:n])
		s = s[n:]
	}
	return pieces
}

func split(sc scanner, s string) []string {
	t := classes()
	var pieces []string
	for i := 0; i < len(s); {
		end := sc(t, s, i)
		pieces = append(pieces, s[i:end])
		i = end
	}
	return pieces
}

var scannersUnderTest = []struct {
	name      string
	scan      scanner
	reference []referenceAlt
}{
	{"o200k_base", scanO200k, referenceO200k},
	{"cl100k_base", scanCL100k, referenceCL100k},
}

func checkSplit(t *testing.T, name string, scan scanner, reference []referenceAlt, s string) bool {
	t.Helper()
	got, want := split(scan, s), referenceSplit(reference, s)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s splits %+q into %+q, the pattern splits it into %+q", name, s, got, want)
		return false
	}
	return true
}

// TestScannersSplitKnownText pins the splits the encodings' behavior turns on,
// each with the reason the pattern gives it. These are literals, not the
// reference's output.
func TestScannersSplitKnownText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		o200k []string
		cl100 []string
	}{
		{"words take one leading space", "hello world", []string{"hello", " world"}, []string{"hello", " world"}},
		{"digits come in threes and take no space", "x 12345", []string{"x", " ", "123", "45"}, []string{"x", " ", "123", "45"}},
		{"contractions stay on the word in o200k only", "can't", []string{"can't"}, []string{"can", "'t"}},
		{"contractions fold case", "CAN'T", []string{"CAN'T"}, []string{"CAN", "'T"}},
		{"a typographic apostrophe is no contraction", "can\U00002019t", []string{"can", "\U00002019t"}, []string{"can", "\U00002019t"}},
		{"long s folds to s", "it'\U0000017f", []string{"it'\U0000017f"}, []string{"it", "'\U0000017f"}},
		{"a capital run followed by lower case is one word", "HTTPServer", []string{"HTTPServer"}, []string{"HTTPServer"}},
		{"upper case then a digit", "ABC1", []string{"ABC", "1"}, []string{"ABC", "1"}},
		{"o200k ends a word where capitals begin if no lower case follows them", "fooBAR baz", []string{"foo", "BAR", " baz"}, []string{"fooBAR", " baz"}},
		{"uncased letters join a word up to the last one", "AB\U00004e2d\U00006587CD", []string{"AB\U00004e2d\U00006587", "CD"}, []string{"AB\U00004e2d\U00006587CD"}},
		{"a tab or ideographic space can lead a word", "\tfoo\U00003000bar", []string{"\tfoo", "\U00003000bar"}, []string{"\tfoo", "\U00003000bar"}},
		{"a line break cannot", "\nfoo", []string{"\n", "foo"}, []string{"\n", "foo"}},
		{"a combining mark can lead a word", "\U00000301abc", []string{"\U00000301abc"}, []string{"\U00000301abc"}},
		{"a mark that fails as a prefix is retried as part of the word", "\U00000301\U00000301 1", []string{"\U00000301\U00000301", " ", "1"}, []string{"\U00000301\U00000301", " ", "1"}},
		{"punctuation takes a leading space and trailing line breaks", "a !!\n\nb", []string{"a", " !!\n\n", "b"}, []string{"a", " !!\n\n", "b"}},
		{"o200k lets a slash trail punctuation", "!/\n/x", []string{"!/\n/", "x"}, []string{"!/\n", "/x"}},
		{"whitespace leaves one space for the next word", "a    b", []string{"a", "   ", " b"}, []string{"a", "   ", " b"}},
		{"whitespace up to the last line break", "a \n \n b", []string{"a", " \n \n", " b"}, []string{"a", " \n \n", " b"}},
		{"o200k splits trailing whitespace after a line break and cl100k keeps it whole", "a \n  ", []string{"a", " \n", "  "}, []string{"a", " \n  "}},
		{"trailing whitespace without a line break", "a   ", []string{"a", "   "}, []string{"a", "   "}},
		{"a lone space before a digit", "a 1", []string{"a", " ", "1"}, []string{"a", " ", "1"}},
		{"special-token text is ordinary text", "<|endoftext|>", []string{"<|", "endoftext", "|>"}, []string{"<|", "endoftext", "|>"}},
		{"empty", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := split(scanO200k, tc.text); strings.Join(got, "|") != strings.Join(tc.o200k, "|") {
				t.Errorf("o200k_base splits %+q into %+q, want %+q", tc.text, got, tc.o200k)
			}
			if got := split(scanCL100k, tc.text); strings.Join(got, "|") != strings.Join(tc.cl100, "|") {
				t.Errorf("cl100k_base splits %+q into %+q, want %+q", tc.text, got, tc.cl100)
			}
		})
	}
}

// structuralAlphabet has one rune of each class the patterns tell apart, with
// the line breaks, slash and apostrophe they single out: lower case, upper
// case, an uncased letter, a titlecase letter, a modifier letter, a mark, a
// digit, three whitespace runes, punctuation and the s of 's.
var structuralAlphabet = []string{
	"a", "A", "\U00004e2d", "\U000001c5", "\U000002b0", "\U00000301", "1", " ", "\n", "\r", "\t", "\U00003000", "!", "/", "'", "s",
}

// contractionAlphabet is its counterpart for contractions: the letters that
// start and complete the suffixes, in both cases, with the long s and the
// typographic apostrophe that must not count.
var contractionAlphabet = []string{
	"'", "s", "S", "t", "T", "r", "R", "e", "E", "v", "V", "m", "M", "l", "L", "d", "D", "\U0000017f", "\U00002019",
	"a", "A", "1", " ", "\U00004e2d", "\U00000301", "\n",
}

// TestScannersAgreeWithPatternsExhaustively compares the scanners with the
// reference on every string of up to four runes of the structural alphabet.
// Under the race detector it compares every string of up to three runes and one
// of each seven of four, in the order the walk makes them, so the same strings
// are compared every time. Seven shares no factor with the alphabet's sixteen,
// so the strings that are skipped are not those that end in the same few runes.
func TestScannersAgreeWithPatternsExhaustively(t *testing.T) {
	const maxRunes, every = 4, 7
	for _, sc := range scannersUnderTest {
		t.Run(sc.name, func(t *testing.T) {
			failures, longest := 0, 0 // longest counts the strings of maxRunes runes
			var walk func(prefix string, depth int)
			walk = func(prefix string, depth int) {
				if failures >= 10 {
					return
				}
				// A string of the greatest length has no depth left.
				if depth == 0 {
					longest++
				}
				if prefix != "" && (!raceEnabled || depth > 0 || longest%every == 0) && !checkSplit(t, sc.name, sc.scan, sc.reference, prefix) {
					failures++
				}
				if depth == 0 {
					return
				}
				for _, r := range structuralAlphabet {
					walk(prefix+r, depth-1)
				}
			}
			walk("", maxRunes)
		})
	}
}

// TestScannersAgreeWithPatternsOnRandomText covers longer strings, with runes
// from a wider range than the exhaustive alphabets.
func TestScannersAgreeWithPatternsOnRandomText(t *testing.T) {
	wide := append(append([]string{}, structuralAlphabet...), contractionAlphabet...)
	wide = append(wide,
		"\U000000a0", "\U00002028", "\U00000085", "\U0000200b", "\U0000200d", "\U0000feff", "\U00000663", "\U000000b2", "\U00002167", "\U000001c4", "\U000001c6",
		"\U000000df", "\U00000e01", "\U00000e31", "\U00000915", "\U0000093e", "\U0001f600", "\U0001d407", "\U00020000", "\U0000fffd",
		"\x00", "\x7f", "@", "-", "_", ".", ",", "\"", "\\")
	rng := rand.New(rand.NewPCG(1, 2))
	n := scaled(12_000, 1_000, 3_000)
	for _, sc := range scannersUnderTest {
		t.Run(sc.name, func(t *testing.T) {
			failures := 0
			for range n {
				var b strings.Builder
				for range 1 + rng.IntN(18) {
					alphabet := wide
					if rng.IntN(4) == 0 {
						alphabet = contractionAlphabet
					}
					b.WriteString(alphabet[rng.IntN(len(alphabet))])
				}
				if !checkSplit(t, sc.name, sc.scan, sc.reference, b.String()) {
					if failures++; failures >= 10 {
						return
					}
				}
			}
		})
	}
}

// TestScannersTileAnyBytes holds the scanners to their contract on arbitrary
// bytes, which are not text: every piece is non-empty and the pieces tile the
// input.
func TestScannersTileAnyBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for _, sc := range scannersUnderTest {
		t.Run(sc.name, func(t *testing.T) {
			for range 5_000 {
				raw := make([]byte, rng.IntN(40))
				for i := range raw {
					raw[i] = byte(rng.IntN(256))
				}
				text := string(raw)
				var joined strings.Builder
				for _, piece := range split(sc.scan, text) {
					if piece == "" {
						t.Fatalf("empty piece in %+q", text)
					}
					joined.WriteString(piece)
				}
				if joined.String() != text {
					t.Fatalf("pieces of %+q rejoin as %+q", text, joined.String())
				}
			}
		})
	}
}

// TestClassesMatchUnicode spot-checks the class table, so a mistake in building
// it (a wrong stride, a missed range) cannot hide behind scanners that share
// it, and compares white space with the standard's list for every rune.
func TestClassesMatchUnicode(t *testing.T) {
	tbl := classes()
	for _, tc := range []struct {
		r    rune
		want uint8
	}{
		{'a', cLower | cLetter},
		{'A', cUpper | cLetter},
		{'\U000001c5', cUpper | cLetter},          // titlecase
		{'\U000002b0', cUpper | cLower | cLetter}, // modifier letter
		{'\U00004e2d', cUpper | cLower | cLetter}, // other letter
		{'\U00020000', cUpper | cLower | cLetter}, // other letter, supplementary plane
		{'\U00000301', cUpper | cLower},           // a mark, which is not a letter
		{'7', cNumber},
		{'\U00000663', cNumber},
		{'\U00002167', cNumber}, // letter number
		{'\U000000b2', cNumber}, // other number
		{' ', cSpace},
		{'\n', cSpace},
		{'\U000000a0', cSpace},
		{'\U00003000', cSpace},
		{'\u0085', cSpace},
		{'\U0000200b', 0}, // zero width space is not white space
		{'!', 0},
		{'\U0001f600', 0},
		{'\U00000378', 0}, // unassigned
		{0x10FFFF, 0},
		{0x110000, 0}, // beyond Unicode
		{-1, 0},
		{utf8.RuneError, 0},
	} {
		if got := tbl.class(tc.r); got != tc.want {
			t.Errorf("class(%U) = %05b, want %05b", tc.r, got, tc.want)
		}
	}
	for r := rune(0); r < utf8.RuneSelf; r++ {
		if tbl.ascii[r] != tbl.class(r) {
			t.Errorf("ascii[%q] = %05b, but class = %05b", r, tbl.ascii[r], tbl.class(r))
		}
	}
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if isWhiteSpace(r) != (tbl.class(r)&cSpace != 0) {
			t.Fatalf("white space disagrees with the standard's list at %U", r)
		}
	}
}

package estimate

import (
	"fmt"
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

// longest counts the strings of maxRunes runes

// A string of the greatest length has no depth left.

// titlecase
// modifier letter
// other letter
// other letter, supplementary plane
// a mark, which is not a letter

// letter number
// other number

// zero width space is not white space

// unassigned

// beyond Unicode

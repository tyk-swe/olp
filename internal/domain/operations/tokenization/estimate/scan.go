package estimate

import "unicode/utf8"

// The two encodings split text into pieces with a regular expression before
// byte-pair merging, and the pieces decide the token count: merging never
// crosses a piece boundary. The expressions use lookahead, which Go's regexp
// package lacks, so each is written here as a scanner. Both scanners are
// transcriptions of the patterns in tiktoken 0.14.0 (tiktoken_ext/
// openai_public.py), written alternative by alternative in the engine's
// priority order. The comments quote each alternative and explain the
// backtracking the engine would do, because the first alternative that can
// match wins, not the longest.
//
// scan_test.go keeps a second, independent implementation of the patterns on
// top of Go's regexp and checks the scanners against it.

// A scanner returns the end of the piece that starts at s[i], for i < len(s).
// Every piece is non-empty, and pieces tile the input.
type scanner func(t *classTable, s string, i int) int

// contraction returns the width of the contraction suffix at the start of s,
// the part after the apostrophe in 's 't 're 've 'm 'll 'd, or 0. The patterns
// fold case with Unicode rules, so U+017F LATIN SMALL LETTER LONG S, which
// folds to s, also matches. Only the ASCII apostrophe starts a contraction.
func contraction(s string) int {
	if len(s) == 0 {
		return 0
	}
	switch s[0] {
	case 's', 'S', 'd', 'D', 'm', 'M', 't', 'T':
		return 1
	case 'l', 'L':
		if len(s) > 1 && (s[1] == 'l' || s[1] == 'L') {
			return 2
		}
	case 'v', 'V', 'r', 'R':
		if len(s) > 1 && (s[1] == 'e' || s[1] == 'E') {
			return 2
		}
	case 0xC5:
		if len(s) > 1 && s[1] == 0xBF {
			return 2
		}
	}
	return 0
}

// punctuationEnd is the shared core of the punctuation alternative,
// ` ?[^\s\p{L}\p{N}]+`: an optional space and then a run of runes that are
// neither whitespace, letters nor numbers. It returns the end of the run, or
// -1 when there is none. A space that is not followed by such a rune does not
// start a match, since the space is itself whitespace.
func punctuationEnd(t *classTable, s string, i int) int {
	j := i
	if s[j] == ' ' {
		j++
	}
	end := t.skip(s, j, cSpace|cLetter|cNumber)
	if end == j {
		return -1
	}
	return end
}

// whitespace resolves the whitespace alternatives for a piece that starts at a
// whitespace rune. With e the end of the whitespace run, they match, in
// priority order:
//
//	\s++$        the whole run, when it reaches the end of the text
//	\s*[\r\n]    the run up to its last line break; o200k's \s*[\r\n]+ ends at
//	             the same place, and has no \s++$ before it
//	\s+(?!\S)    the run without its last rune, so the last whitespace rune
//	             stays free to lead the next piece, as in " word"
//	\s           a single rune
//
// atEnd says whether the run reaching the end of the text is matched ahead of
// the line-break alternative. In o200k it is matched after, which only makes a
// difference when the run holds a line break.
func whitespace(t *classTable, s string, i int, atEnd bool) int {
	e, lineBreak, last := i, -1, i
	for e < len(s) {
		c, w := t.ascii[s[e]&0x7f], 1
		if s[e] >= utf8.RuneSelf {
			c, w = t.decode(s, e)
		}
		if c&cSpace == 0 {
			break
		}
		if b := s[e]; b == '\n' || b == '\r' {
			lineBreak = e + 1
		}
		last = e
		e += w
	}
	switch {
	case e == len(s) && atEnd:
		return e
	case lineBreak >= 0:
		return lineBreak
	case e == len(s):
		return e
	case last > i:
		return last
	}
	return e
}

// scanCL100k splits for cl100k_base, whose pattern is
//
//	'(?i:[sdmt]|ll|ve|re)|[^\r\n\p{L}\p{N}]?+\p{L}++|\p{N}{1,3}+|
//	 ?[^\s\p{L}\p{N}]++[\r\n]*+|\s++$|\s*[\r\n]|\s+(?!\S)|\s
//
// Every possessive quantifier in it is equivalent to a greedy one, because
// nothing after them can fail and force a backtrack.
func scanCL100k(t *classTable, s string, i int) int {
	c, w := t.look(s, i)
	// '(?i:[sdmt]|ll|ve|re)
	if s[i] == '\'' {
		if n := contraction(s[i+1:]); n > 0 {
			return i + 1 + n
		}
	}
	// [^\r\n\p{L}\p{N}]?+\p{L}++: a word with at most one leading rune that is
	// not a letter, number or line break. Whitespace and marks qualify.
	j := i
	if c&(cLetter|cNumber) == 0 && s[i] != '\r' && s[i] != '\n' {
		j += w
	}
	if j < len(s) {
		if next, _ := t.look(s, j); next&cLetter != 0 {
			return t.run(s, j, cLetter)
		}
	}
	// \p{N}{1,3}+: digits split into groups of at most three.
	if c&cNumber != 0 {
		e := i
		for n := 0; n < 3 && e < len(s); n++ {
			digit, dw := t.look(s, e)
			if digit&cNumber == 0 {
				break
			}
			e += dw
		}
		return e
	}
	// ` ?[^\s\p{L}\p{N}]++[\r\n]*+`: punctuation and symbols, with the line
	// breaks that follow them.
	if e := punctuationEnd(t, s, i); e >= 0 {
		for e < len(s) && (s[e] == '\n' || s[e] == '\r') {
			e++
		}
		return e
	}
	if c&cSpace != 0 {
		return whitespace(t, s, i, true)
	}
	// Every rune is matched above, but a scanner must always advance, whatever
	// the table says.
	return i + w
}

// scanO200k splits for o200k_base, whose pattern is the alternation of
//
//	[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?
//	[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?
//	\p{N}{1,3}
//	 ?[^\s\p{L}\p{N}]+[\r\n/]*
//	\s*[\r\n]+
//	\s+(?!\S)
//	\s+
//
// Unlike cl100k's, these quantifiers are not possessive, so the engine can
// give characters back. The word alternatives below say how.
func scanO200k(t *classTable, s string, i int) int {
	c, w := t.look(s, i)
	// The optional prefix may be any rune that is not a letter, number or
	// line break, including whitespace and, because marks are not letters,
	// marks. A mark is also a word rune, so when the word fails after the
	// engine takes the mark as its prefix, it retries with the mark in the
	// word. Each alternative is tried with the prefix first, and then without.
	prefix := c&(cLetter|cNumber) == 0 && s[i] != '\r' && s[i] != '\n'
	word := c&(cUpper|cLower) != 0
	if prefix {
		if e := mixedCase(t, s, i+w); e >= 0 {
			return withContraction(s, e)
		}
	}
	if word {
		if e := mixedCase(t, s, i); e >= 0 {
			return withContraction(s, e)
		}
	}
	if prefix {
		if e := upperFirst(t, s, i+w); e >= 0 {
			return withContraction(s, e)
		}
	}
	if word {
		if e := upperFirst(t, s, i); e >= 0 {
			return withContraction(s, e)
		}
	}
	// \p{N}{1,3}
	if c&cNumber != 0 {
		e := i
		for n := 0; n < 3 && e < len(s); n++ {
			digit, dw := t.look(s, e)
			if digit&cNumber == 0 {
				break
			}
			e += dw
		}
		return e
	}
	// ` ?[^\s\p{L}\p{N}]+[\r\n/]*`: slashes are symbols, so they are already
	// in the run, but the run can end at a line break and resume with one.
	if e := punctuationEnd(t, s, i); e >= 0 {
		for e < len(s) && (s[e] == '\n' || s[e] == '\r' || s[e] == '/') {
			e++
		}
		return e
	}
	if c&cSpace != 0 {
		return whitespace(t, s, i, false)
	}
	// Unreachable, as in scanCL100k.
	return i + w
}

// withContraction extends a word that ends at e with the contraction that
// follows it, (?i:'s|'t|'re|'ve|'m|'ll|'d)?.
func withContraction(s string, e int) int {
	if e < len(s) && s[e] == '\'' {
		if n := contraction(s[e+1:]); n > 0 {
			return e + 1 + n
		}
	}
	return e
}

// mixedCase matches [\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+
// at s[i:], returning its end or -1: a run that holds a lower-case rune,
// where an upper-case prefix is optional. The greedy first class takes every
// rune that can be in it, so the second class gets the next rune if it is
// lower case ("Hello", "HTTPServer"). Otherwise the first class gives runes
// back until the second class can take one. Only runes in both classes, which
// are uncased letters and marks, can do that, and the last of them wins, so
// "AB中文X" matches through 中文 but not X, and "ABC" does not match at all.
func mixedCase(t *classTable, s string, i int) int {
	e, shared := i, -1
	for e < len(s) {
		c, w := t.ascii[s[e]&0x7f], 1
		if s[e] >= utf8.RuneSelf {
			c, w = t.decode(s, e)
		}
		if c&cUpper == 0 {
			break
		}
		e += w
		if c&cLower != 0 {
			shared = e
		}
	}
	if e < len(s) {
		if next, _ := t.look(s, e); next&cLower != 0 {
			return t.run(s, e, cLower)
		}
	}
	return shared
}

// upperFirst matches [\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*
// at s[i:], returning its end or -1: a run that starts with an upper-case or
// uncased rune, for words like "ABC" that mixedCase refused. No backtracking is
// needed, because the second class is optional.
func upperFirst(t *classTable, s string, i int) int {
	e := t.run(s, i, cUpper)
	if e == i {
		return -1
	}
	return t.run(s, e, cLower)
}

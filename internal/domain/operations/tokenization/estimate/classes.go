package estimate

import (
	"sync"
	"unicode"
	"unicode/utf8"
)

// Character classes the pre-tokenizer patterns are written in. A rune carries
// every class it belongs to, so one table lookup answers each \p{..} test.
const (
	// cUpper is [\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}], the first class of the o200k
	// word patterns.
	cUpper uint8 = 1 << iota
	// cLower is [\p{Ll}\p{Lm}\p{Lo}\p{M}], their second class. Letters with no
	// case, and marks, belong to both.
	cLower
	// cLetter is \p{L}. Marks are not letters.
	cLetter
	// cNumber is \p{N}.
	cNumber
	// cSpace is \s, which in the pattern engine tiktoken uses is the Unicode
	// White_Space property rather than Go's ASCII-only Perl class.
	cSpace
)

// classTable maps every code point to its classes. The ASCII half is a flat
// array because most prompts are mostly ASCII; the rest is a two-stage table
// of 256-rune blocks, which stays small because most blocks are uniform.
type classTable struct {
	ascii  [utf8.RuneSelf]uint8
	index  [unicode.MaxRune>>8 + 1]uint16
	blocks [][256]uint8
}

// classes builds the table on first use, so a process that never estimates
// pays nothing for it.
var classes = sync.OnceValue(func() *classTable {
	t := &classTable{blocks: make([][256]uint8, 1, 768)}
	for _, c := range []struct {
		table *unicode.RangeTable
		bits  uint8
	}{
		{unicode.Lu, cUpper | cLetter},
		{unicode.Lt, cUpper | cLetter},
		{unicode.Ll, cLower | cLetter},
		{unicode.Lm, cUpper | cLower | cLetter},
		{unicode.Lo, cUpper | cLower | cLetter},
		{unicode.M, cUpper | cLower},
		{unicode.N, cNumber},
		{unicode.White_Space, cSpace},
	} {
		for _, r := range c.table.R16 {
			for cp := rune(r.Lo); cp <= rune(r.Hi); cp += rune(r.Stride) {
				t.set(cp, c.bits)
			}
		}
		for _, r := range c.table.R32 {
			for cp := rune(r.Lo); cp <= rune(r.Hi); cp += rune(r.Stride) {
				t.set(cp, c.bits)
			}
		}
	}
	for r := range t.ascii {
		t.ascii[r] = t.class(rune(r))
	}
	return t
})

// set adds classes to a code point. Block 0 stands for "no classes" and is
// shared by every block no category touches.
func (t *classTable) set(r rune, bits uint8) {
	b := t.index[r>>8]
	if b == 0 {
		t.blocks = append(t.blocks, [256]uint8{})
		b = uint16(len(t.blocks) - 1)
		t.index[r>>8] = b
	}
	t.blocks[b][r&0xff] |= bits
}

func (t *classTable) class(r rune) uint8 {
	if uint32(r) > unicode.MaxRune {
		return 0
	}
	return t.blocks[t.index[r>>8]][r&0xff]
}

// look returns the classes and width of the rune at s[i]. An invalid byte
// decodes as U+FFFD, a symbol, and one byte wide, so every byte of the input
// belongs to exactly one piece. The ASCII case is split off so that it inlines
// into the scanners' loops.
func (t *classTable) look(s string, i int) (class uint8, width int) {
	if b := s[i]; b < utf8.RuneSelf {
		return t.ascii[b], 1
	}
	return t.decode(s, i)
}

func (t *classTable) decode(s string, i int) (class uint8, width int) {
	r, width := utf8.DecodeRuneInString(s[i:])
	return t.class(r), width
}

// run returns the end of the longest run of runes from i that carry any class
// in mask. The loops here and in the scanners test for ASCII themselves, since
// the call that would do it for them is too large to inline.
func (t *classTable) run(s string, i int, mask uint8) int {
	for i < len(s) {
		c, w := t.ascii[s[i]&0x7f], 1
		if s[i] >= utf8.RuneSelf {
			c, w = t.decode(s, i)
		}
		if c&mask == 0 {
			break
		}
		i += w
	}
	return i
}

// skip returns the end of the longest run of runes from i that carry no class
// in mask.
func (t *classTable) skip(s string, i int, mask uint8) int {
	for i < len(s) {
		c, w := t.ascii[s[i]&0x7f], 1
		if s[i] >= utf8.RuneSelf {
			c, w = t.decode(s, i)
		}
		if c&mask != 0 {
			break
		}
		i += w
	}
	return i
}

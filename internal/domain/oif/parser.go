package oif

import (
	"encoding/json"
	"strconv"
	"strings"
)

type parser struct {
	d  *document
	at int
}

func (p *parser) fail(code string) error { return &SourceError{code, p.at} }

func (p *parser) space() {
	for p.at < len(p.d.raw) {
		switch p.d.raw[p.at] {
		case ' ', '\n', '\r', '\t':
			p.at++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (int, error) {
	p.space()
	if depth > p.d.limits.MaxDepth {
		return 0, p.fail("depth_limit")
	}
	if len(p.d.nodes) >= p.d.limits.MaxNodes {
		return 0, p.fail("node_limit")
	}
	if p.at >= len(p.d.raw) {
		return 0, p.fail("unexpected_end")
	}
	index := len(p.d.nodes)
	n := node{start: p.at}
	p.d.nodes = append(p.d.nodes, n)
	var err error
	switch p.d.raw[p.at] {
	case '{':
		n.kind = Object
		n.members, err = p.objectMembers(depth)
	case '[':
		n.kind = Array
		n.elements, err = p.arrayElements(depth)
	case '"':
		n.kind = String
		err = p.string()
	case 'n':
		n.kind = Null
		if !p.literal("null") {
			err = p.fail("invalid_literal")
		}
	case 't':
		n.kind = Boolean
		if !p.literal("true") {
			err = p.fail("invalid_literal")
		}
	case 'f':
		n.kind = Boolean
		if !p.literal("false") {
			err = p.fail("invalid_literal")
		}
	default:
		n.kind = Number
		err = p.number()
	}
	if err != nil {
		return 0, err
	}
	n.end = p.at
	p.d.nodes[index] = n
	return index, nil
}

func (p *parser) objectMembers(depth int) ([]member, error) {
	p.at++
	p.space()
	if p.take('}') {
		return nil, nil
	}
	var members []member
	seen := map[string]bool{}
	for {
		p.space()
		start := p.at
		if err := p.string(); err != nil {
			return nil, err
		}
		name := p.d.raw[start+1 : p.at-1]
		if strings.ContainsRune(name, '\\') {
			_ = json.Unmarshal([]byte(p.d.raw[start:p.at]), &name)
		}
		if seen[name] {
			return nil, p.fail("duplicate_key")
		}
		seen[name] = true
		p.space()
		if !p.take(':') {
			return nil, p.fail("expected_colon")
		}
		child, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		members = append(members, member{name, child})
		p.space()
		if p.take('}') {
			return members, nil
		}
		if !p.take(',') {
			return nil, p.fail("expected_object_end")
		}
	}
}

func (p *parser) arrayElements(depth int) ([]int, error) {
	p.at++
	p.space()
	if p.take(']') {
		return nil, nil
	}
	var elements []int
	for {
		child, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		elements = append(elements, child)
		p.space()
		if p.take(']') {
			return elements, nil
		}
		if !p.take(',') {
			return nil, p.fail("expected_array_end")
		}
	}
}

func (p *parser) take(c byte) bool {
	if p.at < len(p.d.raw) && p.d.raw[p.at] == c {
		p.at++
		return true
	}
	return false
}

func (p *parser) literal(s string) bool {
	if strings.HasPrefix(p.d.raw[p.at:], s) {
		p.at += len(s)
		return true
	}
	return false
}

func (p *parser) digits() int {
	start := p.at
	for p.at < len(p.d.raw) && p.d.raw[p.at] >= '0' && p.d.raw[p.at] <= '9' {
		p.at++
	}
	return p.at - start
}

func (p *parser) number() error {
	p.take('-')
	if p.at >= len(p.d.raw) {
		return p.fail("invalid_number")
	}
	if !p.take('0') {
		if p.d.raw[p.at] < '1' || p.d.raw[p.at] > '9' {
			return p.fail("invalid_number")
		}
		p.digits()
	}
	if p.take('.') && p.digits() == 0 {
		return p.fail("invalid_number")
	}
	if p.take('e') || p.take('E') {
		if !p.take('+') {
			p.take('-')
		}
		if p.digits() == 0 {
			return p.fail("invalid_number")
		}
	}
	return nil
}

func (p *parser) hex() (uint64, error) {
	if len(p.d.raw)-p.at < 4 {
		return 0, p.fail("invalid_unicode_escape")
	}
	n, err := strconv.ParseUint(p.d.raw[p.at:p.at+4], 16, 16)
	if err != nil {
		return 0, p.fail("invalid_unicode_escape")
	}
	p.at += 4
	return n, nil
}

// plainEnd is the index of the first byte of s from i on that a string cannot
// hold as it is, which is a quotation mark, a backslash or a control character,
// and len(s) if there is none. The text of a prompt is nearly all plain bytes
// and is hundreds of kilobytes of them, so it is read eight bytes at a time and
// only the word that holds a byte of another kind is read one by one.
func plainEnd(s string, i int) int {
	const ones, highs = 0x0101010101010101, 0x8080808080808080
	for ; len(s)-i >= 8; i += 8 {
		b := s[i : i+8]
		w := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
			uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
		// A word is flagged if a byte of it is below 0x20 or is a quotation mark
		// or a backslash. Each test is exact about whether any byte of the word is
		// of its kind, so a flag that only a neighbouring byte raised costs a
		// read of the word and no more.
		quote, slash := w^(ones*'"'), w^(ones*'\\')
		if ((w-ones*0x20)&^w|(quote-ones)&^quote|(slash-ones)&^slash)&highs != 0 {
			break
		}
	}
	for ; i < len(s); i++ {
		if c := s[i]; c == '"' || c == '\\' || c < 0x20 {
			break
		}
	}
	return i
}

func (p *parser) string() error {
	if !p.take('"') {
		return p.fail("expected_string")
	}
	for {
		p.at = plainEnd(p.d.raw, p.at)
		if p.at >= len(p.d.raw) {
			break
		}
		c := p.d.raw[p.at]
		p.at++
		if c == '"' {
			return nil
		}
		if c < 0x20 {
			return p.fail("string_control_character")
		}
		if c != '\\' {
			continue
		}
		if p.at >= len(p.d.raw) {
			return p.fail("invalid_escape")
		}
		c = p.d.raw[p.at]
		p.at++
		switch c {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			n, err := p.hex()
			if err != nil {
				return err
			}
			if n >= 0xDC00 && n <= 0xDFFF {
				return p.fail("unpaired_surrogate")
			}
			if n >= 0xD800 && n <= 0xDBFF {
				if !p.take('\\') || !p.take('u') {
					return p.fail("unpaired_surrogate")
				}
				low, err := p.hex()
				if err != nil {
					return err
				}
				if low < 0xDC00 || low > 0xDFFF {
					return p.fail("unpaired_surrogate")
				}
			}
		default:
			return p.fail("invalid_escape")
		}
	}
	return p.fail("unterminated_string")
}

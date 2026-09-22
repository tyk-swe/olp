// Package oif owns source-preserving, in-process operation contracts. It does
// not depend on a provider dialect, transport, or durable-state implementation.
package oif

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Kind uint8

const (
	Absent Kind = iota
	Null
	Boolean
	Number
	String
	Array
	Object
)

// Limits bounds bytes, nesting and total values independently. Zero selects
// the default; callers may supply their already-enforced transport byte cap.
type Limits struct{ MaxBytes, MaxDepth, MaxNodes int }

func (l Limits) effective() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = 64 << 20
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = 128
	}
	if l.MaxNodes <= 0 {
		l.MaxNodes = 1 << 20
	}
	return l
}

// SourceError never includes source text or object names, which may be secret.
type SourceError struct {
	Code   string
	Offset int
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("invalid source document: %s at byte %d", e.Code, e.Offset)
}

type member struct {
	name  string
	index int
}
type node struct {
	start, end int
	kind       Kind
	members    []member
	elements   []int
}
type document struct {
	raw    string
	nodes  []node
	limits Limits
}

// Document and Value expose no writable aliases to their single source.
type Document struct{ data *document }
type Value struct {
	data  *document
	index int
}
type Member struct {
	Name  string
	Value Value
}

func ParseJSON(data []byte, limits Limits) (Document, error) {
	limits = limits.effective()
	if len(data) > limits.MaxBytes {
		return Document{}, &SourceError{Code: "byte_limit"}
	}
	if !utf8.Valid(data) {
		return Document{}, &SourceError{Code: "invalid_utf8"}
	}
	d := &document{raw: string(data), limits: limits}
	p := parser{d: d}
	if _, err := p.value(0); err != nil {
		return Document{}, err
	}
	p.space()
	if p.at != len(d.raw) {
		return Document{}, p.fail("trailing_data")
	}
	return Document{d}, nil
}

func (d Document) Valid() bool { return d.data != nil }
func (d Document) Len() int {
	if d.data == nil {
		return 0
	}
	return len(d.data.raw)
}
func (d Document) Bytes() []byte {
	if d.data == nil {
		return nil
	}
	return []byte(d.data.raw)
}
func (d Document) Raw() string {
	if d.data == nil {
		return ""
	}
	return d.data.raw
}
func (d Document) Root() Value { return Value{data: d.data} }
func (d Document) Limits() Limits {
	if d.data == nil {
		return Limits{}.effective()
	}
	return d.data.limits
}
func (v Value) Kind() Kind {
	if v.data == nil {
		return Absent
	}
	return v.data.nodes[v.index].kind
}
func (v Value) Raw() string {
	if v.data == nil {
		return ""
	}
	n := v.data.nodes[v.index]
	return v.data.raw[n.start:n.end]
}
func (v Value) Bytes() []byte {
	if v.data == nil {
		return nil
	}
	return []byte(v.Raw())
}
func (v Value) Text() (string, bool) {
	if v.Kind() != String {
		return "", false
	}
	var s string
	if json.Unmarshal(v.Bytes(), &s) != nil {
		return "", false
	}
	return s, true
}
func (v Value) Lookup(name string) (Value, bool) {
	if v.Kind() != Object {
		return Value{}, false
	}
	for _, m := range v.data.nodes[v.index].members {
		if m.name == name {
			return Value{v.data, m.index}, true
		}
	}
	return Value{}, false
}
func (v Value) Elements() []Value {
	if v.Kind() != Array {
		return nil
	}
	indices := v.data.nodes[v.index].elements
	out := make([]Value, len(indices))
	for i, index := range indices {
		out[i] = Value{v.data, index}
	}
	return out
}
func (v Value) Members() []Member {
	if v.Kind() != Object {
		return nil
	}
	entries := v.data.nodes[v.index].members
	out := make([]Member, len(entries))
	for i, m := range entries {
		out[i] = Member{m.name, Value{v.data, m.index}}
	}
	return out
}

// Fields is a compatibility copy. Codecs may rewrite it without changing source.
func (d Document) Fields() map[string]json.RawMessage {
	if d.Root().Kind() != Object {
		return nil
	}
	out := make(map[string]json.RawMessage, len(d.data.nodes[0].members))
	for _, m := range d.data.nodes[0].members {
		out[m.name] = Value{d.data, m.index}.Bytes()
	}
	return out
}
func Pointer(parent, name string) string {
	return parent + "/" + strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
}
func (d Document) Lookup(pointer string) (Value, bool) {
	v := d.Root()
	if pointer == "" {
		return v, d.Valid()
	}
	if !strings.HasPrefix(pointer, "/") {
		return Value{}, false
	}
	for _, part := range strings.Split(pointer[1:], "/") {
		var b strings.Builder
		for i := 0; i < len(part); i++ {
			if part[i] != '~' {
				b.WriteByte(part[i])
				continue
			}
			i++
			if i >= len(part) || part[i] != '0' && part[i] != '1' {
				return Value{}, false
			}
			if part[i] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
		}
		name := b.String()
		if v.Kind() == Array {
			if name == "" || len(name) > 1 && name[0] == '0' {
				return Value{}, false
			}
			for i := 0; i < len(name); i++ {
				if name[i] < '0' || name[i] > '9' {
					return Value{}, false
				}
			}
			i, err := strconv.Atoi(name)
			indices := v.data.nodes[v.index].elements
			if err != nil || i < 0 || i >= len(indices) {
				return Value{}, false
			}
			v = Value{v.data, indices[i]}
		} else {
			var ok bool
			v, ok = v.Lookup(name)
			if !ok {
				return Value{}, false
			}
		}
	}
	return v, true
}

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
	switch p.d.raw[p.at] {
	case '{':
		n.kind = Object
		p.at++
		p.space()
		if p.take('}') {
			break
		}
		seen := map[string]bool{}
		for {
			p.space()
			start := p.at
			if err := p.string(); err != nil {
				return 0, err
			}
			name := p.d.raw[start+1 : p.at-1]
			if strings.ContainsRune(name, '\\') {
				_ = json.Unmarshal([]byte(p.d.raw[start:p.at]), &name)
			}
			if seen[name] {
				return 0, p.fail("duplicate_key")
			}
			seen[name] = true
			p.space()
			if !p.take(':') {
				return 0, p.fail("expected_colon")
			}
			child, err := p.value(depth + 1)
			if err != nil {
				return 0, err
			}
			n.members = append(n.members, member{name, child})
			p.space()
			if p.take('}') {
				break
			}
			if !p.take(',') {
				return 0, p.fail("expected_object_end")
			}
		}
	case '[':
		n.kind = Array
		p.at++
		p.space()
		if p.take(']') {
			break
		}
		for {
			child, err := p.value(depth + 1)
			if err != nil {
				return 0, err
			}
			n.elements = append(n.elements, child)
			p.space()
			if p.take(']') {
				break
			}
			if !p.take(',') {
				return 0, p.fail("expected_array_end")
			}
		}
	case '"':
		n.kind = String
		if err := p.string(); err != nil {
			return 0, err
		}
	case 'n':
		n.kind = Null
		if !p.literal("null") {
			return 0, p.fail("invalid_literal")
		}
	case 't':
		n.kind = Boolean
		if !p.literal("true") {
			return 0, p.fail("invalid_literal")
		}
	case 'f':
		n.kind = Boolean
		if !p.literal("false") {
			return 0, p.fail("invalid_literal")
		}
	default:
		n.kind = Number
		if err := p.number(); err != nil {
			return 0, err
		}
	}
	n.end = p.at
	p.d.nodes[index] = n
	return index, nil
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
func (p *parser) string() error {
	if !p.take('"') {
		return p.fail("expected_string")
	}
	for p.at < len(p.d.raw) {
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

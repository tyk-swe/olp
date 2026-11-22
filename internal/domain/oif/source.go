// Package oif owns source-preserving, in-process operation contracts. It does
// not depend on a provider dialect, transport, or durable-state implementation.
package oif

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
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
		// The index includes per-value metadata and container indexes. Bound
		// its default memory independently of compact scalar input sizes.
		l.MaxNodes = 1 << 16
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
func (v Value) Bounds() (int, int) {
	if v.data == nil {
		return 0, 0
	}
	return v.data.nodes[v.index].start, v.data.nodes[v.index].end
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

// Text is the decoded text of a string value, in memory of its own.
func (v Value) Text() (string, bool) {
	text, shared, ok := v.text()
	if shared {
		text = strings.Clone(text)
	}
	return text, ok
}

// Chars is the decoded text of a string value, which for a string with no
// escape in it is the document's own bytes, read without a copy. It holds the
// whole document up for as long as it is kept, so it is for reading, and Text is
// for keeping.
func (v Value) Chars() (string, bool) {
	text, _, ok := v.text()
	return text, ok
}

// text decodes a string value of a document that ParseJSON accepted, whose
// strings hold only the escapes JSON has and only surrogates that are paired,
// so decoding it takes one pass and cannot fail. A string with no escape is its
// own bytes, and shared says the text is part of the document.
func (v Value) text() (text string, shared, ok bool) {
	if v.Kind() != String {
		return "", false, false
	}
	raw := v.Raw()
	body := raw[1 : len(raw)-1]
	i := strings.IndexByte(body, '\\')
	if i < 0 {
		return body, true, true
	}
	var out strings.Builder
	out.Grow(len(body))
	for i >= 0 {
		out.WriteString(body[:i])
		c := body[i+1]
		body = body[i+2:]
		switch c {
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'u':
			r := hexRune(body[:4])
			body = body[4:]
			if utf16.IsSurrogate(r) {
				// The second half of a pair, which the parser required.
				r = utf16.DecodeRune(r, hexRune(body[2:6]))
				body = body[6:]
			}
			out.WriteRune(r)
		default:
			// A quotation mark, a backslash or a solidus stands for itself.
			out.WriteByte(c)
		}
		i = strings.IndexByte(body, '\\')
	}
	out.WriteString(body)
	return out.String(), false, true
}

// hexRune is the value of four hexadecimal digits that the parser checked.
func hexRune(digits string) rune {
	n, _ := strconv.ParseUint(digits, 16, 16)
	return rune(n)
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
func (d Document) Fields() map[string]json.RawMessage { return d.FieldsWithout() }

// FieldsWithout is Fields without the named members, for a reader that reads
// them from the document and would otherwise copy them, a prompt of 400 KB
// among them, for nothing.
func (d Document) FieldsWithout(names ...string) map[string]json.RawMessage {
	if d.Root().Kind() != Object {
		return nil
	}
	out := make(map[string]json.RawMessage, len(d.data.nodes[0].members))
	for _, m := range d.data.nodes[0].members {
		if !slices.Contains(names, m.name) {
			out[m.name] = Value{d.data, m.index}.Bytes()
		}
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

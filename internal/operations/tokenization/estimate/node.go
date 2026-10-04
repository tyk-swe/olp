package estimate

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/oif"
)

// node is one JSON value as the walker reads it. A request's prompt is read from
// its document, which is parsed and checked once for the whole gateway, so a
// value of it is found by its place in the document and its text is read where
// it lies: a prompt of 400 KB is not scanned again for each level of the
// request it is nested in. What a provider's defaults hold is raw JSON, which has
// not been checked, and is decoded as it is read. The zero node is no value at
// all, and every reading of it finds nothing, as every reading of a value of
// another shape does.
type node struct {
	value oif.Value
	raw   json.RawMessage
}

// object is a node read as the members of an object, and the zero object is
// what a node of any other shape reads as.
type object struct {
	value   oif.Value
	members map[string]json.RawMessage
}

// valueNode is a value of a request's document.
func valueNode(v oif.Value) node { return node{value: v} }

// rawNode is raw JSON, and no value when there is none.
func rawNode(raw json.RawMessage) node { return node{raw: raw} }

// present reports whether there is a value, null included.
func (n node) present() bool { return n.value.Kind() != oif.Absent || len(n.raw) > 0 }

// isNull reports whether the value is the literal null.
func (n node) isNull() bool {
	if n.value.Kind() != oif.Absent {
		return n.value.Kind() == oif.Null
	}
	return string(n.raw) == "null"
}

// source is the JSON text of the value, which is what a member such as a block's
// type is compared by, spelling and all.
func (n node) source() string {
	if n.value.Kind() != oif.Absent {
		return n.value.Raw()
	}
	return string(n.raw)
}

// bytes is the JSON text of the value for a reading that takes bytes, which for
// a value of a document is a copy.
func (n node) bytes() json.RawMessage {
	if n.value.Kind() != oif.Absent {
		return n.value.Bytes()
	}
	return n.raw
}

// text reads a JSON string, reporting whether the value was one. As encoding/json
// reads it, null is a string with nothing in it. The text of a value of a document
// that has no escape in it is the document's own bytes, which only a reading that
// is done with it before the document is may keep, so view says the text is one.
func (n node) text() (text string, view, ok bool) {
	if n.value.Kind() != oif.Absent {
		if n.value.Kind() == oif.Null {
			return "", false, true
		}
		text, ok = n.value.Chars()
		return text, ok, ok
	}
	text, ok = textOf(n.raw)
	return text, false, ok
}

// array reads the value as an array, reporting whether it was one. As
// encoding/json reads it, null is an array of nothing.
func (n node) array() ([]node, bool) {
	if n.value.Kind() != oif.Absent {
		switch n.value.Kind() {
		case oif.Null:
			return nil, true
		case oif.Array:
			values := n.value.Elements()
			items := make([]node, len(values))
			for i, v := range values {
				items[i] = valueNode(v)
			}
			return items, true
		}
		return nil, false
	}
	// What is not there is answered before the variable that would hold it is
	// declared: one that is decoded into is allocated whether or not it is.
	if len(n.raw) == 0 {
		return nil, false
	}
	var raws []json.RawMessage
	if json.Unmarshal(n.raw, &raws) != nil {
		return nil, false
	}
	items := make([]node, len(raws))
	for i, raw := range raws {
		items[i] = rawNode(raw)
	}
	return items, true
}

// elements are the items of an array, and none for a value of any other shape.
func (n node) elements() []node {
	items, _ := n.array()
	return items
}

// object reads the value as an object, which is not one for a value of any other
// shape, null included.
func (n node) object() object {
	if n.value.Kind() != oif.Absent {
		if n.value.Kind() == oif.Object {
			return object{value: n.value}
		}
		return object{}
	}
	return object{members: jsonObject(n.raw)}
}

// exists reports whether the node was an object, empty or not.
func (o object) exists() bool { return o.value.Kind() == oif.Object || o.members != nil }

// get is a member, and no value for one the object does not have.
func (o object) get(name string) node {
	if o.value.Kind() == oif.Object {
		v, _ := o.value.Lookup(name)
		return valueNode(v)
	}
	return rawNode(o.members[name])
}

package protocols

import (
	"bytes"
	"encoding/json"
	"strings"
)

// schemaObject is a JSON object that keeps its members in the order they were
// written, and every scalar and name as it was written: Go's maps keep neither,
// and the order of a schema's properties is the order a structured output
// follows. A node is a schemaObject, a list, or a scalar's json.RawMessage.
type schemaObject []schemaMember

type schemaMember struct {
	name    string
	written json.RawMessage // the name with its quotes, as written
	value   any
}

// jsonSchemaTypes rewrites the type names of Gemini's OpenAPI-subset schema,
// which are capitals (OBJECT, STRING), to the lower-case names of JSON Schema
// that every other provider reads. The rest of the schema is left alone, down
// to the order and spelling of its members, and a schema with nothing to
// rewrite keeps its bytes.
func jsonSchemaTypes(schema json.RawMessage) json.RawMessage {
	if !present(schema) {
		return schema
	}
	reader := &schemaReader{decoder: json.NewDecoder(bytes.NewReader(schema)), data: schema}
	node, err := reader.node()
	if err != nil || !lowerSchemaTypes(node) {
		return schema
	}
	var out bytes.Buffer
	writeSchemaNode(&out, node)
	return out.Bytes()
}

// schemaReader reads the nodes of a schema, and keeps the bytes of each token.
type schemaReader struct {
	decoder *json.Decoder
	data    []byte
	offset  int64
}

// token is the next token and its bytes: what lies between the end of the
// previous token and the end of this one, less the separators and whitespace
// that no token begins with.
func (r *schemaReader) token() (json.Token, json.RawMessage, error) {
	token, err := r.decoder.Token()
	if err != nil {
		return nil, nil, err
	}
	end := r.decoder.InputOffset()
	written := bytes.TrimLeft(r.data[r.offset:end], " \t\r\n,:")
	r.offset = end
	return token, written, nil
}

func (r *schemaReader) node() (any, error) {
	token, written, err := r.token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return written, nil
	}
	if delim == '[' {
		list := []any{}
		for r.decoder.More() {
			item, err := r.node()
			if err != nil {
				return nil, err
			}
			list = append(list, item)
		}
		_, _, err := r.token()
		return list, err
	}
	object := schemaObject{}
	for r.decoder.More() {
		name, written, err := r.token()
		if err != nil {
			return nil, err
		}
		value, err := r.node()
		if err != nil {
			return nil, err
		}
		object = append(object, schemaMember{name: name.(string), written: written, value: value})
	}
	_, _, err = r.token()
	return object, err
}

// writeSchemaNode writes a node a schemaReader read, without whitespace.
func writeSchemaNode(out *bytes.Buffer, node any) {
	switch node := node.(type) {
	case schemaObject:
		out.WriteByte('{')
		for i, member := range node {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(member.written)
			out.WriteByte(':')
			writeSchemaNode(out, member.value)
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, item := range node {
			if i > 0 {
				out.WriteByte(',')
			}
			writeSchemaNode(out, item)
		}
		out.WriteByte(']')
	case json.RawMessage:
		out.Write(node)
	}
}

// lowerSchemaTypes lowercases the capitalized type names of a schema node and
// its sub-schemas, and reports whether it changed any. A name written twice is
// lowered at each of its members, so that a reader of either one sees the
// standard names.
func lowerSchemaTypes(node any) bool {
	schema, ok := node.(schemaObject)
	if !ok {
		return false
	}
	changed := false
	for i, member := range schema {
		switch member.name {
		case "type":
			if written, ok := member.value.(json.RawMessage); ok {
				var name string
				if json.Unmarshal(written, &name) == nil {
					switch name {
					case "STRING", "NUMBER", "INTEGER", "BOOLEAN", "ARRAY", "OBJECT", "NULL":
						schema[i].value = json.RawMessage(`"` + strings.ToLower(name) + `"`)
						changed = true
					}
				}
			}
		case "properties", "$defs", "definitions", "patternProperties":
			if named, ok := member.value.(schemaObject); ok {
				for _, sub := range named {
					changed = lowerSchemaTypes(sub.value) || changed
				}
			}
		case "items", "additionalProperties", "not":
			if tuple, ok := member.value.([]any); ok {
				for _, sub := range tuple {
					changed = lowerSchemaTypes(sub) || changed
				}
			} else {
				changed = lowerSchemaTypes(member.value) || changed
			}
		case "anyOf", "oneOf", "allOf", "prefixItems":
			if subs, ok := member.value.([]any); ok {
				for _, sub := range subs {
					changed = lowerSchemaTypes(sub) || changed
				}
			}
		}
	}
	return changed
}

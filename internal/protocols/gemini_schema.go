package protocols

import (
	"bytes"
	"encoding/json"
	"slices"
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

// geminiSchemaMembers are the members of Gemini's Schema, the OpenAPI subset
// its parameters and responseSchema fields take. Those fields refuse any other
// member of a schema ("Unknown name"), which JSON Schema documents routinely
// carry: $schema, additionalProperties, $ref, const, oneOf.
var geminiSchemaMembers = map[string]bool{
	"type": true, "format": true, "title": true, "description": true, "nullable": true, "enum": true,
	"maxItems": true, "minItems": true, "properties": true, "required": true,
	"minProperties": true, "maxProperties": true, "minLength": true, "maxLength": true,
	"pattern": true, "example": true, "anyOf": true, "propertyOrdering": true, "default": true,
	"items": true, "minimum": true, "maximum": true,
}

// inGeminiSchema reports whether a schema is one Gemini's OpenAPI-subset fields
// take: an object whose members, and those of every schema inside it, are
// members of Gemini's Schema, with a single type name and an enum of strings. A
// schema outside it is sent in the fields that take JSON Schema as it is,
// parametersJsonSchema and responseJsonSchema, which Gemini's own SDKs use for
// it. A schema inside it goes where it always did.
func inGeminiSchema(schema json.RawMessage) bool {
	var members map[string]json.RawMessage
	if json.Unmarshal(schema, &members) != nil {
		return false
	}
	for name, value := range members {
		if !geminiSchemaMembers[name] {
			return false
		}
		switch name {
		case "type":
			if !bytes.HasPrefix(value, []byte(`"`)) {
				return false
			}
		case "enum":
			// Gemini's enum is a list of strings, which JSON Schema's is not.
			var values []json.RawMessage
			if json.Unmarshal(value, &values) != nil || slices.ContainsFunc(values, func(v json.RawMessage) bool { return !bytes.HasPrefix(v, []byte(`"`)) }) {
				return false
			}
		case "items":
			if !inGeminiSchema(value) {
				return false
			}
		case "anyOf":
			var branches []json.RawMessage
			if json.Unmarshal(value, &branches) != nil || slices.ContainsFunc(branches, func(b json.RawMessage) bool { return !inGeminiSchema(b) }) {
				return false
			}
		case "properties":
			var properties map[string]json.RawMessage
			if json.Unmarshal(value, &properties) != nil {
				return false
			}
			for _, property := range properties {
				if !inGeminiSchema(property) {
					return false
				}
			}
		}
	}
	return true
}

// geminiSchemaField is the member of a Gemini request that carries a schema: the
// OpenAPI-subset field when the schema is in the subset, and the field that takes
// JSON Schema when it is not.
func geminiSchemaField(schema json.RawMessage, subset, jsonSchema string) string {
	if inGeminiSchema(schema) {
		return subset
	}
	return jsonSchema
}

// jsonSchemaTypes rewrites the type names of Gemini's OpenAPI-subset schema,
// which are capitals (OBJECT, STRING), to the lower-case names of JSON Schema
// that every other provider reads. The rest of the schema is left alone, down
// to the order and spelling of its members, and a schema with nothing to
// rewrite keeps its bytes.
func jsonSchemaTypes(schema json.RawMessage) json.RawMessage {
	if !present(schema) || !mayNameCapitalTypes(schema) {
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

// capitalTypeNames are the type names of Gemini's schema as they are written.
var capitalTypeNames = [][]byte{[]byte(`"STRING"`), []byte(`"NUMBER"`), []byte(`"INTEGER"`), []byte(`"BOOLEAN"`), []byte(`"ARRAY"`), []byte(`"OBJECT"`), []byte(`"NULL"`)}

// mayNameCapitalTypes is false for a schema that has no type name to rewrite,
// as nearly every schema from a client that writes JSON Schema has none: reading
// one only to write it back costs about a hundred allocations a tool. A name can
// be written without spelling out its capitals only through a \u escape, so a
// schema that has one is read.
func mayNameCapitalTypes(schema json.RawMessage) bool {
	if bytes.Contains(schema, []byte(`\u`)) {
		return true
	}
	return slices.ContainsFunc(capitalTypeNames, func(name []byte) bool { return bytes.Contains(schema, name) })
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

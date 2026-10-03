package scripted

import (
	"encoding/json"
	"math"
	"strings"
)

// sample builds the deterministic instance of a JSON schema that structured
// output and forced tool calls answer with. It supports the keywords the
// official SDKs and agent frameworks emit: types, enums, constants, objects,
// arrays, local references, and the any-of, one-of and all-of combinators.
// Unsupported keywords are ignored, which only ever widens what is valid.
func sample(schema json.RawMessage) any {
	var root any
	if len(schema) == 0 || json.Unmarshal(schema, &root) != nil {
		return map[string]any{}
	}
	return sampleNode(root, root, 0)
}

const maxSampleDepth = 12

func sampleNode(root, node any, depth int) any {
	obj, ok := node.(map[string]any)
	if !ok || depth > maxSampleDepth {
		return nil
	}
	if ref, ok := obj["$ref"].(string); ok {
		return sampleNode(root, resolve(root, ref), depth+1)
	}
	if v, ok := obj["const"]; ok {
		return v
	}
	if enum, ok := obj["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if branches, ok := obj[key].([]any); ok && len(branches) > 0 {
			for _, branch := range branches {
				if b, ok := branch.(map[string]any); !ok || b["type"] != "null" {
					return sampleNode(root, branch, depth+1)
				}
			}
			return nil
		}
	}
	if parts, ok := obj["allOf"].([]any); ok && len(parts) > 0 {
		merged := map[string]any{}
		for _, part := range parts {
			if m, ok := sampleNode(root, part, depth+1).(map[string]any); ok {
				for k, v := range m {
					merged[k] = v
				}
			}
		}
		return merged
	}
	kind := schemaType(obj)
	switch kind {
	case "string":
		return sampleString(obj)
	case "integer":
		return clamp(obj, 42, true)
	case "number":
		return clamp(obj, 4.5, false)
	case "boolean":
		return true
	case "null":
		return nil
	case "array":
		count := 1
		if min, ok := obj["minItems"].(float64); ok && int(min) > count {
			count = int(min)
		}
		items := make([]any, 0, count)
		for i := 0; i < count; i++ {
			items = append(items, sampleNode(root, obj["items"], depth+1))
		}
		return items
	default:
		out := map[string]any{}
		props, _ := obj["properties"].(map[string]any)
		for name, prop := range props {
			out[name] = sampleNode(root, prop, depth+1)
		}
		return out
	}
}

// schemaType returns the first non-null type, inferring object when only
// properties are given.
func schemaType(obj map[string]any) string {
	switch t := obj["type"].(type) {
	case string:
		return strings.ToLower(t)
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s != "null" {
				return strings.ToLower(s)
			}
		}
		return "null"
	}
	if _, ok := obj["properties"]; ok {
		return "object"
	}
	if _, ok := obj["items"]; ok {
		return "array"
	}
	return "object"
}

func sampleString(obj map[string]any) string {
	var text string
	switch obj["format"] {
	case "date-time":
		text = "2025-01-01T00:00:00Z"
	case "date":
		text = "2025-01-01"
	case "time":
		text = "00:00:00Z"
	case "email":
		text = "fixture@example.com"
	case "uri", "url":
		text = "https://example.com/"
	case "uuid":
		text = "00000000-0000-4000-8000-000000000000"
	default:
		text = "fixture"
	}
	if min, ok := obj["minLength"].(float64); ok && int(min) > len(text) {
		text += strings.Repeat("x", int(min)-len(text))
	}
	if max, ok := obj["maxLength"].(float64); ok && int(max) < len(text) && max >= 0 {
		text = text[:int(max)]
	}
	return text
}

func clamp(obj map[string]any, value float64, integer bool) any {
	if min, ok := obj["minimum"].(float64); ok && value < min {
		value = min
	}
	if min, ok := obj["exclusiveMinimum"].(float64); ok && value <= min {
		value = min + 1
	}
	if max, ok := obj["maximum"].(float64); ok && value > max {
		value = max
	}
	if max, ok := obj["exclusiveMaximum"].(float64); ok && value >= max {
		value = max - 1
	}
	if integer {
		return int64(math.Round(value))
	}
	return value
}

// resolve follows a local JSON Pointer reference such as #/$defs/Item.
func resolve(root any, ref string) any {
	pointer, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil
	}
	node := root
	for _, segment := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if segment == "" {
			continue
		}
		segment = strings.NewReplacer("~1", "/", "~0", "~").Replace(segment)
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[segment]
	}
	return node
}

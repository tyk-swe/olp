package embeddings

import (
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func defaults(id string) map[string]operations.Field {
	fields := map[string]operations.Field{}
	boolean := operations.FieldSchema(map[string]any{"type": "boolean"}, func(v oif.Value) error {
		if v.Kind() != oif.Boolean {
			return operations.Invalid("default", "Use a boolean.")
		}
		return nil
	})
	nullableBoolean := operations.FieldSchema(map[string]any{"type": []string{"boolean", "null"}}, operations.NullableBool)
	positive := operations.FieldSchema(map[string]any{"type": []string{"integer", "null"}, "minimum": 1}, operations.PositiveInt)
	text := operations.FieldSchema(map[string]any{"type": []string{"string", "null"}}, func(v oif.Value) error {
		if !operations.Optional(v) && v.Kind() != oif.String {
			return operations.Invalid("default", "Use text or native null.")
		}
		return nil
	})
	enum := func(values ...string) operations.Field {
		return operations.FieldSchema(map[string]any{"type": "string", "enum": values}, func(v oif.Value) error {
			for _, value := range values {
				if operations.String(v) == value && v.Kind() == oif.String {
					return nil
				}
			}
			return operations.Invalid("default", "The native option is outside the declared enum.")
		})
	}
	switch id {
	case "openai-embeddings":
		fields["dimensions"] = positive
		fields["encoding_format"] = enum("float", "base64")
	case "voyage-embeddings":
		fields["output_dimension"] = positive
		fields["truncation"] = boolean
		fields["output_dtype"] = enum("float", "int8", "uint8", "binary", "ubinary")
		fields["input_type"] = operations.FieldSchema(map[string]any{"type": []string{"string", "null"}, "enum": []any{"query", "document", nil}}, func(v oif.Value) error {
			if v.Kind() == oif.Null || operations.String(v) == "query" || operations.String(v) == "document" {
				return nil
			}
			return operations.Invalid("input_type", "Use query, document or native null.")
		})
		fields["encoding_format"] = operations.FieldSchema(map[string]any{"type": []string{"string", "null"}, "enum": []any{"base64", nil}}, func(v oif.Value) error {
			if v.Kind() == oif.Null || operations.String(v) == "base64" {
				return nil
			}
			return operations.Invalid("encoding_format", "Use base64 or native null.")
		})
	case "gemini-embeddings":
		fields["outputDimensionality"] = positive
		fields["taskType"] = text
		fields["title"] = text
	case "gemini-batch-embeddings":
		// Every batch member owns its controls; no root default can stand in for them.
	case "vertex-embeddings":
		fields["parameters"] = operations.FieldSchema(map[string]any{"type": "object"}, func(v oif.Value) error {
			if v.Kind() != oif.Object {
				return operations.Invalid("parameters", "Use a native parameter object.")
			}
			return nil
		})
	case "bedrock-embeddings":
		fields["dimensions"] = positive
		fields["normalize"] = boolean
		fields["embeddingTypes"] = operations.FieldSchema(map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"enum": []string{"float", "binary"}}}, func(v oif.Value) error {
			if v.Kind() != oif.Array || len(v.Elements()) == 0 {
				return operations.Invalid("embeddingTypes", "Use native embedding types.")
			}
			for _, e := range v.Elements() {
				if operations.String(e) != "float" && operations.String(e) != "binary" {
					return operations.Invalid("embeddingTypes", "Use float or binary.")
				}
			}
			return nil
		})
	default:
		fields["truncate"] = nullableBoolean
		fields["truncation_direction"] = enum("left", "right", "Left", "Right")
		fields["prompt_name"] = text
		if id == "tei-embeddings" {
			fields["normalize"] = boolean
			fields["dimensions"] = positive
		}
	}
	return fields
}

func inputText(request oif.Request, id string) ([]operations.Text, error) {
	if strings.HasPrefix(id, "tei-") {
		v := operations.Member(request.Document().Root(), "prompt_name")
		if !operations.Optional(v) {
			return nil, operations.Error("policy_conflict", "/prompt_name", "native_prefix_scope", "The selected provider prefix is not available to this input policy.")
		}
	}
	known := map[string]bool{inputField(id): true, "model": true, "user": true}
	for name := range defaults(id) {
		known[name] = true
	}
	for _, field := range request.Document().Root().Members() {
		if !known[field.Name] {
			return nil, operations.Error("policy_conflict", "/native_extension", "input_policy_coverage", "The input policy has no inspection contract for an unknown native embedding control.")
		}
	}
	out := []operations.Text{}
	var visit func(oif.Value, string)
	visit = func(value oif.Value, path string) {
		switch value.Kind() {
		case oif.String:
			out = append(out, operations.Text{Pointer: path, Value: operations.String(value)})
		case oif.Array:
			for index, child := range value.Elements() {
				visit(child, operations.Pointer(path, strconv.Itoa(index)))
			}
		case oif.Object:
			for _, field := range value.Members() {
				if field.Name != "model" {
					visit(field.Value, operations.Pointer(path, field.Name))
				}
			}
		}
	}
	for _, name := range []string{inputField(id), "input_type", "prompt_name", "title"} {
		if value, present := request.Document().Root().Lookup(name); present {
			visit(value, "/"+name)
		}
	}
	if strings.HasPrefix(id, "gemini-") {
		for _, text := range out {
			if strings.Contains(text.Pointer, "inlineData") || strings.Contains(text.Pointer, "fileData") {
				return nil, operations.Error("policy_conflict", "/content", "input_policy_coverage", "This text policy cannot inspect native media embedding inputs.")
			}
		}
	}
	return out, nil
}

package connectors

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func defaultSchema(profileID, dialect, operation string) json.RawMessage {
	codec, registered := operationregistry.Lookup(dialect)
	fields := map[string]any{}
	for _, name := range defaultFields(dialect, operation) {
		if profileID == "tgi-completions" && !slices.Contains(tgiCompletionFields, name) {
			continue
		}
		fields[name] = defaultControlSchema(dialect, operation, name)
		if field, found := codec.Defaults[name]; registered && found {
			fields[name] = field.Schema
		}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": []string{"dialect"}, "properties": map[string]any{
		"dialect": map[string]any{"const": dialect}, "values": map[string]any{"type": "object", "additionalProperties": false, "properties": fields}, "native_options": map[string]any{"type": "object", "description": "Operation payload extensions; routing, credentials and resource references are reserved."},
	}})
	return schema
}

func defaultControlSchema(dialect, operation, name string) map[string]any {
	kind := defaultControlKind(operation, name)
	schema := map[string]any{"title": name, "type": []string{kind, "null"}}
	if operation == "translation" {
		if name == "response_format" {
			schema["enum"] = slices.Collect(func(yield func(any) bool) {
				for _, format := range wire.TranslationResponseFormats() {
					if !yield(format) {
						return
					}
				}
				yield(nil)
			})
		}
		if name == "temperature" {
			schema["minimum"], schema["maximum"] = 0, 1
		}
	}
	if kind == "array" || kind == "object" {
		schema["description"] = "Replaced atomically; never recursively merged."
	}
	if kind == "integer" {
		schema["minimum"] = 1
	}
	if dialect == "openai-completions" {
		switch name {
		case "logprobs":
			schema["type"], schema["minimum"], schema["maximum"] = []string{"integer", "null"}, 0, 5
		case "best_of":
			schema["type"], schema["minimum"] = []string{"integer", "null"}, 1
		case "seed":
			schema["type"] = []string{"integer", "null"}
		case "max_tokens":
			schema["minimum"] = 0
		case "echo":
			schema["type"] = []string{"boolean", "null"}
		case "logit_bias":
			schema["type"] = []string{"object", "null"}
		case "stop":
			schema["type"] = []string{"string", "array", "null"}
			schema["items"] = map[string]any{"type": "string"}
		}
	}
	return schema
}

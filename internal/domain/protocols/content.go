package protocols

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

type TextSlot func(text string) (next string, stop bool)

type inspector struct {
	fn    TextSlot
	stop  bool
	depth int
	err   error
}

const inspectMaxDepth = 64

// InputInspectable reports whether InspectInputText walks the prompt text of a
// family. Callers enforcing input content policy must not dispatch a body whose
// family it cannot inspect.
func InputInspectable(family wire.Format) bool {
	return (&inspector{}).inputFields(family, nil)
}

// InspectInputText inspects declared input text and structured tool data. An
// unsafe rewrite fails closed; callers must not dispatch a request on error.
func InspectInputText(r *wire.Request, fn TextSlot) (*wire.Request, error) {
	fields := r.Document()
	w := &inspector{fn: fn}
	w.inputFields(r.Family, fields)
	// Configured tool/schema text is part of the effective invocation too. Arrays
	// remain ordered and atomic; the explicit mutation policy controls text values.
	for _, name := range []string{"tools", "functions", "toolConfig", "response_format"} {
		w.field(fields, name, w.stringValues)
	}
	if w.err != nil {
		return nil, w.err
	}
	return r.WithFields(fields)
}

// inputFields walks the declared prompt text of a family's body and reports
// whether it knows the family. InputInspectable calls it with no fields, so the
// gate and the walk cannot drift apart.
func (w *inspector) inputFields(family wire.Format, fields map[string]json.RawMessage) bool {
	switch family {
	case wire.Completions:
		if fields != nil {
			if !textInput(fields["prompt"]) {
				w.err = errors.New("token-ID completion prompts have no text inspection contract")
				return true
			}
		}
		w.field(fields, "prompt", w.stringOrList)
	case wire.Chat:
		w.field(fields, "messages", func(raw json.RawMessage) json.RawMessage {
			return w.messageList(raw, inspectOpenAIMessage)
		})
	case wire.Responses, wire.InputTokens:
		w.field(fields, "instructions", w.text)
		w.field(fields, "input", w.responsesInput)
	case wire.Embeddings, wire.Moderation:
		w.field(fields, "input", w.stringOrList)
	case wire.Rerank:
		w.field(fields, "query", w.text)
		w.field(fields, "documents", w.stringOrList)
	case wire.BedrockRerank:
		w.field(fields, "queries", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(item *json.RawMessage) {
				*item = w.object(*item, func(query map[string]json.RawMessage) {
					w.path(query, w.text, "textQuery", "text")
				})
			})
		})
		w.field(fields, "sources", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(item *json.RawMessage) {
				*item = w.object(*item, func(source map[string]json.RawMessage) {
					w.path(source, w.text, "inlineDocumentSource", "textDocument", "text")
				})
			})
		})
	case wire.Anthropic, wire.AnthropicCount:
		w.field(fields, "system", w.textOrParts)
		w.field(fields, "messages", func(raw json.RawMessage) json.RawMessage {
			return w.messageList(raw, inspectAnthropicMessage)
		})
	case wire.Gemini, wire.GeminiStream:
		w.geminiFields(fields)
	case wire.GeminiCount:
		if _, ok := fields["generateContentRequest"]; ok {
			w.field(fields, "generateContentRequest", func(raw json.RawMessage) json.RawMessage {
				return w.object(raw, w.geminiFields)
			})
		} else {
			w.geminiFields(fields)
		}
	case wire.Bedrock:
		w.bedrockFields(fields)
	case wire.BedrockCount:
		w.field(fields, "input", func(raw json.RawMessage) json.RawMessage {
			return w.object(raw, func(input map[string]json.RawMessage) {
				w.field(input, "converse", func(raw json.RawMessage) json.RawMessage {
					return w.object(raw, w.bedrockFields)
				})
			})
		})
	case wire.GeminiEmbeddings:
		w.field(fields, "title", w.text)
		w.field(fields, "content", func(raw json.RawMessage) json.RawMessage {
			return w.object(raw, w.geminiParts)
		})
	case wire.GeminiEmbeddingsBatch:
		w.field(fields, "requests", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(item *json.RawMessage) {
				*item = w.object(*item, func(req map[string]json.RawMessage) {
					w.field(req, "title", w.text)
					w.field(req, "content", func(raw json.RawMessage) json.RawMessage {
						return w.object(raw, w.geminiParts)
					})
				})
			})
		})
	case wire.VertexEmbeddings:
		w.field(fields, "instances", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(item *json.RawMessage) {
				*item = w.object(*item, func(instance map[string]json.RawMessage) {
					w.field(instance, "title", w.text)
					w.field(instance, "content", w.text)
				})
			})
		})
	case wire.BedrockEmbeddings:
		w.field(fields, "inputText", w.text)
	default:
		return false
	}
	return true
}

func (w *inspector) stringValues(raw json.RawMessage) json.RawMessage {
	if w.stop || w.depth >= inspectMaxDepth {
		return raw
	}
	if _, ok := rawString(raw); ok {
		return w.text(raw)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw
	}
	w.depth++
	defer func() { w.depth-- }()
	switch trimmed[0] {
	case '[':
		return w.list(raw, func(value *json.RawMessage) { *value = w.stringValues(*value) })
	case '{':
		return w.object(raw, func(fields map[string]json.RawMessage) {
			for name, value := range fields {
				fields[name] = w.stringValues(value)
			}
		})
	}
	return raw
}

// structured inspects all data positions, including object names and non-string
// scalars. The source parser bounds nesting, so this walk must not silently skip
// valid input beyond the shallower limit used by text-content containers.
func (w *inspector) structured(raw json.RawMessage) json.RawMessage {
	if w.stop {
		return raw
	}
	doc, err := oif.ParseJSON(raw, oif.Limits{})
	if err != nil {
		w.err = errors.New("structured tool data could not be inspected")
		w.stop = true
		return raw
	}
	return w.structuredValue(doc.Root())
}

func (w *inspector) structuredValue(value oif.Value) json.RawMessage {
	raw := value.Bytes()
	if w.stop {
		return raw
	}
	switch value.Kind() {
	case oif.String:
		return w.text(raw)
	case oif.Number, oif.Boolean, oif.Null:
		next, stop := w.fn(value.Raw())
		if stop {
			w.stop = true
			return raw
		}
		if next == value.Raw() {
			return raw
		}
		// Keep a primitive's type when the replacement is valid for that type.
		// Otherwise encode it as string data, never as raw JSON syntax.
		if replacement, err := oif.ParseJSON([]byte(next), oif.Limits{}); err == nil && replacement.Root().Kind() == value.Kind() {
			return replacement.Bytes()
		}
		encoded, _ := json.Marshal(next)
		return encoded
	case oif.Array:
		items := value.Elements()
		out := make([]json.RawMessage, len(items))
		for i, item := range items {
			out[i] = w.structuredValue(item)
			if w.stop {
				return raw
			}
		}
		encoded, _ := json.Marshal(out)
		return encoded
	case oif.Object:
		out := make(map[string]json.RawMessage, len(value.Members()))
		for _, field := range value.Members() {
			name, stop := w.fn(field.Name)
			if stop {
				w.stop = true
				return raw
			}
			if _, exists := out[name]; exists {
				w.err = errors.New("input content policy rewrite produced duplicate object names")
				w.stop = true
				return raw
			}
			out[name] = w.structuredValue(field.Value)
			if w.stop {
				return raw
			}
		}
		encoded, _ := json.Marshal(out)
		return encoded
	}
	return raw
}

func InspectOutputText(family wire.Format, body []byte, fn TextSlot) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body, errors.New("response body is not a JSON object")
	}
	w := &inspector{fn: fn}
	switch family {
	case wire.Completions:
		w.field(fields, "choices", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(choice *json.RawMessage) {
				*choice = w.object(*choice, func(c map[string]json.RawMessage) {
					if text, ok := c["text"]; ok {
						c["text"] = w.text(text)
					}
				})
			})
		})
	case wire.Chat:
		w.field(fields, "choices", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(choice *json.RawMessage) {
				*choice = w.object(*choice, func(c map[string]json.RawMessage) {
					if m, ok := c["message"]; ok {
						c["message"] = w.object(m, w.openAIOutputMessage)
					}
				})
			})
		})
	case wire.Responses:
		w.field(fields, "output", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(item *json.RawMessage) {
				*item = w.object(*item, func(o map[string]json.RawMessage) {
					itemType, _ := rawString(o["type"])
					if itemType != "message" {
						return
					}
					if c, ok := o["content"]; ok {
						o["content"] = w.textOrParts(c)
					}
				})
			})
		})
	case wire.Anthropic:
		w.field(fields, "content", w.textOrParts)
	case wire.Gemini, wire.GeminiStream:
		w.field(fields, "candidates", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(item *json.RawMessage) {
				*item = w.object(*item, func(c map[string]json.RawMessage) {
					if content, ok := c["content"]; ok {
						c["content"] = w.object(content, w.geminiOutputParts)
					}
				})
			})
		})
	default:
		return body, nil
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return body, errors.New("response body could not be rewritten")
	}
	return out, nil
}

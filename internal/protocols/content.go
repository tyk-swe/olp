package protocols

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
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
func InputInspectable(family openai.Family) bool {
	switch family {
	case openai.FamilyChat, openai.FamilyResponses, openai.FamilyInputTokens,
		openai.FamilyEmbeddings, openai.FamilyModeration, openai.FamilyRerank, openai.FamilyBedrockRerank,
		openai.FamilyAnthropic, openai.FamilyAnthropicCount,
		openai.FamilyGemini, openai.FamilyGeminiStream, openai.FamilyGeminiCount,
		openai.FamilyBedrock, "bedrock_count",
		openai.FamilyGeminiEmbeddings, openai.FamilyGeminiEmbeddingsBatch,
		openai.FamilyVertexEmbeddings, openai.FamilyBedrockEmbeddings:
		return true
	}
	return false
}

// InspectInputText inspects declared input text and structured tool data. An
// unsafe rewrite fails closed; callers must not dispatch a request on error.
func InspectInputText(r *openai.Request, fn TextSlot) (*openai.Request, error) {
	fields := r.Document()
	w := &inspector{fn: fn}
	switch r.Family {
	case openai.FamilyChat:
		w.field(fields, "messages", func(raw json.RawMessage) json.RawMessage {
			return w.messageList(raw, inspectOpenAIMessage)
		})
	case openai.FamilyResponses, openai.FamilyInputTokens:
		w.field(fields, "instructions", w.text)
		w.field(fields, "input", w.responsesInput)
	case openai.FamilyEmbeddings, openai.FamilyModeration:
		w.field(fields, "input", w.stringOrList)
	case openai.FamilyRerank:
		w.field(fields, "query", w.text)
		w.field(fields, "documents", w.stringOrList)
	case openai.FamilyAnthropic, openai.FamilyAnthropicCount:
		w.field(fields, "system", w.textOrParts)
		w.field(fields, "messages", func(raw json.RawMessage) json.RawMessage {
			return w.messageList(raw, inspectAnthropicMessage)
		})
	case openai.FamilyGemini, openai.FamilyGeminiStream:
		w.geminiFields(fields)
	case openai.FamilyGeminiCount:
		if _, ok := fields["generateContentRequest"]; ok {
			w.field(fields, "generateContentRequest", func(raw json.RawMessage) json.RawMessage {
				return w.object(raw, w.geminiFields)
			})
		} else {
			w.geminiFields(fields)
		}
	case openai.FamilyBedrock:
		w.bedrockFields(fields)
	case "bedrock_count":
		w.field(fields, "input", func(raw json.RawMessage) json.RawMessage {
			return w.object(raw, func(input map[string]json.RawMessage) {
				w.field(input, "converse", func(raw json.RawMessage) json.RawMessage {
					return w.object(raw, w.bedrockFields)
				})
			})
		})
	case openai.FamilyGeminiEmbeddings:
		w.field(fields, "title", w.text)
		w.field(fields, "content", func(raw json.RawMessage) json.RawMessage {
			return w.object(raw, w.geminiParts)
		})
	case openai.FamilyGeminiEmbeddingsBatch:
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
	case openai.FamilyVertexEmbeddings:
		w.field(fields, "instances", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(item *json.RawMessage) {
				*item = w.object(*item, func(instance map[string]json.RawMessage) {
					w.field(instance, "title", w.text)
					w.field(instance, "content", w.text)
				})
			})
		})
	case openai.FamilyBedrockEmbeddings:
		w.field(fields, "inputText", w.text)
	}
	// Configured tool/schema text is part of the effective invocation too. Arrays
	// remain ordered and atomic; the explicit mutation policy controls text values.
	for _, name := range []string{"tools", "functions", "toolConfig", "response_format"} {
		w.field(fields, name, w.stringValues)
	}
	if w.err != nil {
		return nil, w.err
	}
	out := r.WithFields(fields, oif.ExplicitTransform)
	if !out.OIF().Document().Valid() {
		return nil, errors.New("input content policy rewrite produced an invalid document")
	}
	return out, nil
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

func InspectOutputText(family openai.Family, body []byte, fn TextSlot) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body, errors.New("response body is not a JSON object")
	}
	w := &inspector{fn: fn}
	switch family {
	case openai.FamilyChat:
		w.field(fields, "choices", func(raw json.RawMessage) json.RawMessage {
			return w.list(raw, func(choice *json.RawMessage) {
				*choice = w.object(*choice, func(c map[string]json.RawMessage) {
					if m, ok := c["message"]; ok {
						c["message"] = w.object(m, w.openAIOutputMessage)
					}
				})
			})
		})
	case openai.FamilyResponses:
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
	case openai.FamilyAnthropic:
		w.field(fields, "content", w.textOrParts)
	case openai.FamilyGemini, openai.FamilyGeminiStream:
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

func inspectOpenAIMessage(m map[string]json.RawMessage, w *inspector) {
	if raw, ok := m["content"]; ok {
		m["content"] = w.textOrParts(raw)
	}
	if raw, ok := m["refusal"]; ok {
		m["refusal"] = w.text(raw)
	}
	if raw, ok := m["tool_calls"]; ok {
		m["tool_calls"] = w.list(raw, func(call *json.RawMessage) {
			*call = w.object(*call, func(t map[string]json.RawMessage) {
				if f, ok := t["function"]; ok {
					t["function"] = w.object(f, func(fn map[string]json.RawMessage) {
						if a, ok := fn["arguments"]; ok {
							fn["arguments"] = w.text(a)
						}
					})
				}
			})
		})
	}
}

func (w *inspector) openAIOutputMessage(m map[string]json.RawMessage) {
	if raw, ok := m["content"]; ok {
		m["content"] = w.textOrParts(raw)
	}
	if raw, ok := m["refusal"]; ok {
		m["refusal"] = w.text(raw)
	}
}

func inspectAnthropicMessage(m map[string]json.RawMessage, w *inspector) {
	if raw, ok := m["content"]; ok {
		m["content"] = w.textOrParts(raw)
	}
}

func (w *inspector) responsesInput(raw json.RawMessage) json.RawMessage {
	if w.stop || raw == nil {
		return raw
	}
	if _, ok := rawString(raw); ok {
		return w.text(raw)
	}
	return w.list(raw, func(item *json.RawMessage) {
		*item = w.object(*item, func(o map[string]json.RawMessage) {
			for _, name := range []string{"content", "output"} {
				if v, ok := o[name]; ok {
					o[name] = w.textOrParts(v)
				}
			}
			if v, ok := o["arguments"]; ok {
				o["arguments"] = w.text(v)
			}
		})
	})
}

func (w *inspector) textOrParts(raw json.RawMessage) json.RawMessage {
	if w.stop || raw == nil {
		return raw
	}
	if _, ok := rawString(raw); ok {
		return w.text(raw)
	}
	return w.list(raw, func(part *json.RawMessage) {
		*part = w.object(*part, func(p map[string]json.RawMessage) {
			for _, name := range []string{"text", "refusal"} {
				if v, ok := p[name]; ok {
					p[name] = w.text(v)
				}
			}
			if v, ok := p["content"]; ok && w.depth < inspectMaxDepth {
				w.depth++
				p["content"] = w.textOrParts(v)
				w.depth--
			}
		})
	})
}

func (w *inspector) stringOrList(raw json.RawMessage) json.RawMessage {
	if w.stop || raw == nil {
		return raw
	}
	if _, ok := rawString(raw); ok {
		return w.text(raw)
	}
	return w.list(raw, func(item *json.RawMessage) {
		if _, ok := rawString(*item); ok {
			*item = w.text(*item)
		}
	})
}

func (w *inspector) messageList(raw json.RawMessage, each func(map[string]json.RawMessage, *inspector)) json.RawMessage {
	return w.list(raw, func(item *json.RawMessage) {
		*item = w.object(*item, func(m map[string]json.RawMessage) {
			each(m, w)
		})
	})
}

func (w *inspector) geminiFields(fields map[string]json.RawMessage) {
	w.field(fields, "contents", func(raw json.RawMessage) json.RawMessage {
		return w.list(raw, func(item *json.RawMessage) {
			*item = w.object(*item, w.geminiParts)
		})
	})
	w.field(fields, "systemInstruction", func(raw json.RawMessage) json.RawMessage {
		return w.object(raw, w.geminiParts)
	})
}

// bedrockFields walks a Converse body: system and message text blocks, tool
// results and tool-use input. toolConfig is covered by the generic walk.
func (w *inspector) bedrockFields(fields map[string]json.RawMessage) {
	w.field(fields, "system", w.textOrParts)
	w.field(fields, "messages", func(raw json.RawMessage) json.RawMessage {
		return w.messageList(raw, inspectBedrockMessage)
	})
}

func inspectBedrockMessage(m map[string]json.RawMessage, w *inspector) {
	w.field(m, "content", func(raw json.RawMessage) json.RawMessage {
		return w.list(raw, func(block *json.RawMessage) {
			*block = w.object(*block, func(b map[string]json.RawMessage) {
				w.field(b, "text", w.text)
				w.field(b, "toolUse", func(raw json.RawMessage) json.RawMessage {
					return w.object(raw, func(use map[string]json.RawMessage) {
						w.field(use, "input", w.structured)
					})
				})
				w.field(b, "toolResult", func(raw json.RawMessage) json.RawMessage {
					return w.object(raw, func(result map[string]json.RawMessage) {
						w.field(result, "content", func(raw json.RawMessage) json.RawMessage {
							return w.list(raw, func(item *json.RawMessage) {
								*item = w.object(*item, func(c map[string]json.RawMessage) {
									w.field(c, "text", w.text)
									w.field(c, "json", w.structured)
								})
							})
						})
					})
				})
			})
		})
	})
}

func (w *inspector) geminiParts(obj map[string]json.RawMessage) {
	w.field(obj, "parts", func(raw json.RawMessage) json.RawMessage {
		return w.list(raw, func(part *json.RawMessage) {
			*part = w.object(*part, func(p map[string]json.RawMessage) {
				if v, ok := p["text"]; ok {
					p["text"] = w.text(v)
				}
			})
		})
	})
}

func (w *inspector) geminiOutputParts(obj map[string]json.RawMessage) {
	w.field(obj, "parts", func(raw json.RawMessage) json.RawMessage {
		return w.list(raw, func(part *json.RawMessage) {
			*part = w.object(*part, func(p map[string]json.RawMessage) {
				if thought, ok := p["thought"]; ok && string(bytes.TrimSpace(thought)) == "true" {
					return
				}
				if v, ok := p["text"]; ok {
					p["text"] = w.text(v)
				}
			})
		})
	})
}

func (w *inspector) field(fields map[string]json.RawMessage, name string, transform func(json.RawMessage) json.RawMessage) {
	if w.stop {
		return
	}
	if raw, ok := fields[name]; ok {
		fields[name] = transform(raw)
	}
}

func (w *inspector) text(raw json.RawMessage) json.RawMessage {
	if w.stop {
		return raw
	}
	value, ok := rawString(raw)
	if !ok {
		return raw
	}
	next, stop := w.fn(value)
	if stop {
		w.stop = true
		return raw
	}
	if next == value {
		return raw
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return raw
	}
	return encoded
}

func (w *inspector) object(raw json.RawMessage, each func(map[string]json.RawMessage)) json.RawMessage {
	if w.stop || raw == nil {
		return raw
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return raw
	}
	each(fields)
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

func (w *inspector) list(raw json.RawMessage, each func(*json.RawMessage)) json.RawMessage {
	if w.stop || raw == nil {
		return raw
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return raw
	}
	for i := range items {
		each(&items[i])
		if w.stop {
			return raw
		}
	}
	out, err := json.Marshal(items)
	if err != nil {
		return raw
	}
	return out
}

func rawString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}

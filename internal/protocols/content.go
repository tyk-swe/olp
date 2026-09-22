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
}

const inspectMaxDepth = 64

func InspectInputText(r *openai.Request, fn TextSlot) *openai.Request {
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
	}
	// Configured tool/schema text is part of the effective invocation too. Arrays
	// remain ordered and atomic; the explicit mutation policy controls text values.
	for _, name := range []string{"tools", "functions", "toolConfig", "response_format"} {
		w.field(fields, name, w.stringValues)
	}
	return r.WithFields(fields, oif.ExplicitTransform)
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

package protocols

import (
	"bytes"
	"encoding/json"
)

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

// path walks nested objects along names and transforms the last field.
func (w *inspector) path(fields map[string]json.RawMessage, transform func(json.RawMessage) json.RawMessage, names ...string) {
	if len(names) == 1 {
		w.field(fields, names[0], transform)
		return
	}
	w.field(fields, names[0], func(raw json.RawMessage) json.RawMessage {
		return w.object(raw, func(inner map[string]json.RawMessage) {
			w.path(inner, transform, names[1:]...)
		})
	})
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

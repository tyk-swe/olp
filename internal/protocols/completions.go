package protocols

import (
	"encoding/json"
	"io"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

var completionChatFields = strings.Fields("model prompt max_tokens temperature top_p n stop stream stream_options presence_penalty frequency_penalty logit_bias user seed")

func encodeCompletionChat(r *wire.Request, target wire.Format, kind, vendor, model string, defaults Object, applied *[]oif.Provenance) ([]byte, wire.Format, error) {
	if target != wire.Chat {
		return nil, target, unsupported("completions_mapping")
	}
	f := r.Document()
	var prompt string
	if json.Unmarshal(f["prompt"], &prompt) != nil {
		return nil, target, unsupported("prompt")
	}
	for key := range f {
		if !slices.Contains(completionChatFields, key) {
			return nil, target, unsupported(key)
		}
	}
	for key := range defaults {
		if !slices.Contains(completionChatFields, key) || key == "model" || key == "prompt" || key == "stream" {
			return nil, target, unsupported("defaults/" + key)
		}
	}
	if options, err := optionalObject(f["stream_options"]); err != nil {
		return nil, target, unsupported("stream_options")
	} else {
		for name := range options {
			if name != "include_usage" && name != "include_obfuscation" {
				return nil, target, unsupported("stream_options/" + name)
			}
		}
	}
	delete(f, "prompt")
	// Legacy Completions defaults to 16 output tokens. An omitted or null
	// Chat limit would instead allow the remaining model context.
	if limit, explicit := f["max_tokens"]; explicit && !present(limit) || !explicit && !present(defaults["max_tokens"]) {
		f["max_tokens"] = raw(16)
	}
	f["messages"] = raw([]any{map[string]any{"role": "user", "content": prompt}})
	f["model"] = raw(r.Route)
	parsed, err := openai.Parse(wire.Chat, raw(f))
	if err != nil {
		return nil, target, err
	}
	return encodeTransformed(parsed, target, kind, vendor, model, defaults, applied)
}

// Project every candidate in place. A refusal or tool result has no text-
// completion equivalent and fails rather than being silently discarded.
func projectChatCompletion(body []byte, streaming bool) ([]byte, error) {
	f, err := object(body)
	if err != nil {
		return nil, err
	}
	for key := range f {
		if streaming && key == "obfuscation" {
			continue
		}
		if !slices.Contains(strings.Fields("id object created model choices usage system_fingerprint service_tier"), key) {
			return nil, protocolError("unmapped chat completion field " + key)
		}
	}
	choices := arr(f["choices"])
	for i, v := range choices {
		c, e := object(v)
		if e != nil {
			return nil, e
		}
		name := "message"
		if streaming {
			name = "delta"
		}
		m, e := object(c[name])
		if e != nil {
			return nil, protocolError("chat completion omitted " + name)
		}
		for k, v := range m {
			if k == "role" || k == "content" || (k == "annotations" || k == "tool_calls") && string(v) == "[]" {
				continue
			}
			if present(v) {
				return nil, protocolError("chat " + k + " has no legacy completion mapping")
			}
		}
		for k, v := range c {
			if k != "index" && k != "finish_reason" && k != name && (k != "logprobs" || present(v)) {
				return nil, protocolError("unmapped chat choice field " + k)
			}
		}
		text := ""
		if present(m["content"]) {
			if json.Unmarshal(m["content"], &text) != nil {
				return nil, protocolError("chat content is not text")
			}
		}
		delete(c, name)
		c["text"] = raw(text)
		c["logprobs"] = raw(nil)
		choices[i] = raw(c)
	}
	f["object"] = raw("text_completion")
	f["choices"] = raw(choices)
	return raw(f), nil
}

func streamCompletionChat(r io.Reader, maxEvent int, route string, includeUsage bool, emit wire.Emit, observe func(oif.Event) error) (*wire.Completion, error) {
	return openai.StreamMetadataEvents(wire.Chat, r, maxEvent, route, includeUsage, func(frame []byte) error {
		return sse.Decode(strings.NewReader(string(frame)), max(len(frame), maxEvent), func(f sse.Frame) error {
			if f.Data == "[DONE]" {
				return emit(f.Encode())
			}
			projected, err := projectChatCompletion([]byte(f.Data), true)
			if err != nil {
				return err
			}
			f.Data = string(projected)
			return emit(f.Encode())
		})
	}, observe)
}

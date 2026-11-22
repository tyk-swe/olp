package estimate

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

// dialectMessages walks the conversation of a native Anthropic, Gemini or
// Bedrock request, whose entries are messages in a shape of their own.
func (w walker) dialectMessages(n node) {
	w.dialect(n)
	for _, n := range n.elements() {
		if item := n.object(); item.exists() {
			w.in.messages++
			if role, ok := roleOf(item.get("role")); ok {
				w.in.roles = append(w.in.roles, role)
			}
		}
	}
}

// dialectSystem walks a system prompt, which a model reads as a message.
func (w walker) dialectSystem(n node) {
	w.dialect(n)
	if n.present() && !n.isNull() {
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// dialectRequest walks the request a Gemini count wraps.
func (w walker) dialectRequest(n node) {
	w.dialect(n)
	inner := n.object()
	w.with(toolSchemas).schema(inner.get("tools"))
	w.with(unclassified).generationConfig(inner.get("generationConfig"))
	for _, n := range inner.get("contents").elements() {
		if item := n.object(); item.exists() {
			w.in.messages++
			if role, ok := roleOf(item.get("role")); ok {
				w.in.roles = append(w.in.roles, role)
			}
		}
	}
	if n := inner.get("systemInstruction"); n.present() && !n.isNull() {
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// dialect charges native content the same text and media units as OpenAI's.
// Blob bytes are never mistaken for text tokens.
func (w walker) dialect(n node) {
	if !n.present() || w.text(n) {
		return
	}
	if items, ok := n.array(); ok {
		for _, item := range items {
			w.dialect(item)
		}
		return
	}
	f := n.object()
	if !f.exists() {
		return
	}
	if f.get("inlineData").present() || f.get("fileData").present() || f.get("type").source() == `"image"` {
		w.in.addMedia(ImageTokens)
		return
	}
	switch f.get("type").source() {
	case `"thinking"`:
		// The text the model reasoned in, which it reads again in a tool loop.
		w.with(unclassified).toolText(f.get("thinking"))
		w.in.approx = true
		return
	case `"redacted_thinking"`:
		// Its reasoning, encrypted: nothing a count can read.
		w.in.approx = true
		return
	case `"document"`:
		w.document(f)
		return
	}
	// A tool call or its result, whichever keys carry its text.
	part := dialectCategory(w.category, f)
	if isNativeToolBlock(f) {
		w.in.approx = true
	}
	for _, key := range [...]string{"text", "content", "parts", "contents", "systemInstruction", "functionResponse", "functionCall", "input", "output"} {
		if value := f.get(key); value.present() {
			switch key {
			case "functionResponse", "functionCall", "input", "output":
				w.in.approx = true
				w.with(toolCallsResults).dialect(value)
			case "systemInstruction":
				w.with(instructions).dialect(value)
			default:
				w.with(part).dialect(value)
			}
		}
	}
}

// document walks an Anthropic document block: the text of one given as text, or
// as content blocks, and for any other source, a PDF or a file as base64 or a
// URL, the flat charge of a media part, whatever the bytes it carries.
func (w walker) document(block object) {
	source := block.get("source").object()
	switch source.get("type").source() {
	case `"text"`:
		w.toolText(source.get("data"))
	case `"content"`:
		w.dialect(source.get("content"))
	default:
		w.in.addMedia(MediaTokens)
		return
	}
	w.in.approx = true
}

// OutputBounds reads how much reply a request allows: the bound it named for
// one candidate, and how many candidates it asked for. Defaults supply omitted
// top-level fields. For an effective provider bound, pass the destination
// request with its defaults already merged and nil defaults.
func OutputBounds(request *wire.Request, defaults map[string]json.RawMessage) (output *int64, candidates int64) {
	field := func(name string) json.RawMessage { return fieldOf(request, defaults, name) }
	candidates = 1
	outputFields := []string{"max_completion_tokens", "max_tokens"}
	if request != nil && request.Family == wire.Completions {
		outputFields = []string{"max_tokens"}
	}
	if request != nil && request.Family == wire.Responses {
		outputFields = []string{"max_output_tokens"}
	}
	for _, name := range outputFields {
		if value, ok := integerValue(field(name)); ok {
			output = &value
			break
		}
	}
	if request != nil && request.Family == wire.Bedrock {
		if value, ok := integerValue(jsonObject(field("inferenceConfig"))["maxTokens"]); ok {
			output = &value
		}
	}
	if request != nil && request.Family.Surface() == "gemini" {
		config := jsonObject(field("generationConfig"))
		if v, ok := integerValue(config["maxOutputTokens"]); ok {
			output = &v
		}
		if v, ok := integerValue(config["candidateCount"]); ok {
			candidates = v
		}
	}
	if request == nil || request.Family == wire.Chat || request.Family == wire.Completions {
		if value, ok := integerValue(field("n")); ok {
			candidates = value
		}
	}
	if request != nil && request.Family == wire.Completions {
		if best, ok := integerValue(field("best_of")); ok {
			candidates = max(candidates, best)
		}
		var prompts []json.RawMessage
		if json.Unmarshal(field("prompt"), &prompts) == nil && len(prompts) > 0 && len(prompts[0]) > 0 && (prompts[0][0] == '"' || prompts[0][0] == '[') {
			candidates = multiplyBounded(candidates, int64(len(prompts)))
		}
	}
	return output, candidates
}

// fieldOf reads a field the way the encoder resolves it: the caller's value,
// then the provider's default. Match Encode's precedence, including an explicit
// null opting out of a default and either chat token-bound alias overriding the
// other.
func fieldOf(request *wire.Request, defaults map[string]json.RawMessage, name string) json.RawMessage {
	if request != nil {
		if raw := request.Field(name); len(raw) > 0 {
			return raw
		}
		if request.Family == wire.Chat &&
			((name == "max_tokens" && len(request.Field("max_completion_tokens")) > 0) ||
				(name == "max_completion_tokens" && len(request.Field("max_tokens")) > 0)) {
			return nil
		}
	}
	return defaults[name]
}

// textOf reads a JSON string, reporting whether the value was one.
func textOf(raw json.RawMessage) (string, bool) {
	// What is not there is answered before the variable that would hold it is
	// declared: one that is decoded into is allocated whether or not it is.
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

// jsonObject decodes a value of the shape the estimate expects, and nothing at
// all for any other shape.
func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

// integerValue distinguishes null from an explicit integer.
func integerValue(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value *int64
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return 0, false
	}
	return *value, true
}

// multiplyBounded and addBounded saturate at MaxTokens.
func multiplyBounded(a, b int64) int64 {
	if a > MaxTokens/b {
		return MaxTokens
	}
	return a * b
}

func addBounded(a, b int64) int64 {
	if a > MaxTokens-b {
		return MaxTokens
	}
	return a + b
}

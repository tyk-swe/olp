package estimate

// This file holds the request walker as internal/gateway/limits.go had it
// before it moved into this package, verbatim apart from the names. It is the
// reference TestWalkerMatchesTheLegacyHeuristic holds the walker to: for every
// family without a tokenizer the estimate must stay what the gateway always
// charged, bit for bit.

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

const (
	legacyCharsPerToken       = 4
	legacyImageTokens         = 1000
	legacyMediaTokens         = 2000
	legacyDefaultOutputTokens = 4096
	legacyMaxEstimate         = 1<<53 - 1
)

// legacyEstimateTokens is the tokens a request may consume, charged before the
// upstream reports what it actually used. The prompt is walked rather than
// weighed: text is charged at four characters per token, each media part at a
// flat rate, and the reply at the largest size the caller allowed — the bound
// it named for one candidate, multiplied by the candidates asked for. It is
// deliberately generous — a reservation is reconciled against the real usage
// as soon as the attempt ends, and admitting work that cannot fit in the
// window is worse than deferring work that would have.
func legacyEstimateTokens(parsed *openai.Request, defaults ...map[string]json.RawMessage) int64 {
	input, output, candidates := legacyEstimateParts(parsed, defaults...)
	return legacyEstimateTokensFromParts(parsed, input, output, candidates)
}

func legacyEstimateTokensFromParts(parsed *openai.Request, input int64, output *int64, candidates int64) int64 {
	if parsed != nil && parsed.Family.Operation() != "generation" {
		return max(input, 1)
	}
	bound := int64(legacyDefaultOutputTokens)
	if output != nil {
		bound = *output
	}
	return max(legacyAddBounded(input, legacyMultiplyBounded(max(bound, 1), max(candidates, 1))), 1)
}

func legacyEstimateParts(parsed *openai.Request, defaults ...map[string]json.RawMessage) (input int64, output *int64, candidates int64) {
	// Match Encode's precedence, including explicit null opting out of a
	// default and either chat token-bound alias overriding the other.
	field := func(name string) json.RawMessage {
		if parsed != nil {
			if raw := parsed.Field(name); len(raw) > 0 {
				return raw
			}
			if parsed.Family == openai.FamilyChat &&
				((name == "max_tokens" && len(parsed.Field("max_completion_tokens")) > 0) ||
					(name == "max_completion_tokens" && len(parsed.Field("max_tokens")) > 0)) {
				return nil
			}
		}
		if len(defaults) > 0 {
			return defaults[0][name]
		}
		return nil
	}
	candidates = 1
	if parsed != nil {
		switch parsed.Family {
		case openai.FamilyChat:
			input = legacyEstimateItems(field("messages"))
		case openai.FamilyEmbeddings:
			input = legacyEstimateEmbeddingInput(field("input"))
		case openai.FamilyResponses, openai.FamilyInputTokens, openai.FamilyModeration:
			input = legacyAddBounded(legacyEstimateItems(field("input")), legacyEstimateText(field("instructions")))
		}
		if parsed.Family.Surface() != "openai" {
			input = legacyAddBounded(legacyEstimateNative(field("messages")), legacyEstimateNative(field("system")))
			input = legacyAddBounded(input, legacyEstimateNative(field("contents")))
			input = legacyAddBounded(input, legacyEstimateNative(field("systemInstruction")))
			input = legacyAddBounded(input, legacyEstimateNative(field("generateContentRequest")))
		}
		if parsed.Family.Surface() != "openai" {
			input = legacyAddBounded(input, legacyEstimateSchema(field("tools")))
		} else {
			input = legacyAddBounded(input, legacyEstimateTools(field("tools")))
		}
		if parsed.Family.Operation() != "generation" {
			return max(input, 1), nil, 1
		}
	}
	outputFields := []string{"max_completion_tokens", "max_tokens"}
	if parsed != nil && parsed.Family == openai.FamilyResponses {
		outputFields = []string{"max_output_tokens"}
	}
	for _, name := range outputFields {
		if value, ok := legacyIntegerValue(field(name)); ok {
			output = &value
			break
		}
	}
	if parsed != nil && parsed.Family == openai.FamilyBedrock {
		if value, ok := legacyIntegerValue(legacyJsonObject(field("inferenceConfig"))["maxTokens"]); ok {
			output = &value
		}
	}
	if parsed != nil && parsed.Family.Surface() == "gemini" {
		config := legacyJsonObject(field("generationConfig"))
		if v, ok := legacyIntegerValue(config["maxOutputTokens"]); ok {
			output = &v
		}
		if v, ok := legacyIntegerValue(config["candidateCount"]); ok {
			candidates = v
		}
	}

	if parsed == nil || parsed.Family == openai.FamilyChat {
		if value, ok := legacyIntegerValue(field("n")); ok {
			candidates = value
		}
	}
	return input, output, candidates
}

// legacyEstimateItems charges the conversation one request carries: the messages of
// a chat request, or the input of a responses request, which may also be one
// plain string. A shape this gateway does not recognise is charged nothing
// rather than guessed at, because the reservation is reconciled against the
// usage the upstream reports as soon as the attempt ends.
func legacyEstimateItems(raw json.RawMessage) int64 {
	if tokens, ok := legacyTextTokens(raw); ok {
		return tokens
	}
	total := int64(0)
	for _, raw := range legacyJsonArray(raw) {
		item := legacyJsonObject(raw)
		if item == nil {
			continue
		}
		// A responses tool result carries what it returned in `output`.
		total = legacyAddBounded(total, legacyEstimateContent(item["content"]))
		total = legacyAddBounded(total, legacyEstimateContent(item["output"]))
		// The identifiers and arguments a message travels with are prompt text
		// like any other; `call_id` and `arguments` are the responses spelling
		// of the chat tool-call fields.
		for _, name := range [...]string{"name", "tool_call_id", "call_id", "arguments"} {
			total = legacyAddBounded(total, legacyEstimateText(item[name]))
		}
		for _, raw := range legacyJsonArray(item["tool_calls"]) {
			call := legacyJsonObject(raw)
			if function := legacyJsonObject(call["function"]); function != nil {
				call = function
			}
			total = legacyAddBounded(total, legacyEstimateText(call["name"]))
			total = legacyAddBounded(total, legacyEstimateText(call["arguments"]))
		}
	}
	return total
}

// legacyEstimateContent charges one message body: plain text, or the parts a
// multimodal message carries.
func legacyEstimateContent(raw json.RawMessage) int64 {
	if tokens, ok := legacyTextTokens(raw); ok {
		return tokens
	}
	total := int64(0)
	for _, raw := range legacyJsonArray(raw) {
		part := legacyJsonObject(raw)
		if part == nil {
			// A bare string among the parts is text.
			total = legacyAddBounded(total, legacyEstimateText(raw))
			continue
		}
		kind, _ := legacyTextOf(part["type"])
		switch kind {
		case "image_url", "input_image":
			total = legacyAddBounded(total, legacyImageTokens)
		case "input_audio", "input_file", "file":
			total = legacyAddBounded(total, legacyMediaTokens)
		default:
			total = legacyAddBounded(total, legacyEstimateText(part["text"]))
			total = legacyAddBounded(total, legacyEstimateText(part["refusal"]))
		}
	}
	return total
}

// legacyEstimateTools charges the tool catalogue a request carries: a schema the
// model has to read costs what any other prompt text costs.
func legacyEstimateTools(raw json.RawMessage) int64 {
	total := int64(0)
	for _, raw := range legacyJsonArray(raw) {
		tool := legacyJsonObject(raw)
		// Chat nests the definition under `function`; responses holds it flat.
		if function := legacyJsonObject(tool["function"]); function != nil {
			tool = function
		}
		total = legacyAddBounded(total, legacyEstimateText(tool["name"]))
		total = legacyAddBounded(total, legacyEstimateText(tool["description"]))
		total = legacyAddBounded(total, legacyEstimateSchema(tool["parameters"]))
	}
	return total
}

// legacyEstimateSchema charges a JSON schema as the document it is, with whatever
// formatting the caller happened to send removed.
func legacyEstimateSchema(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return 0
	}
	return legacyCharge(utf8.RuneCount(compact.Bytes()))
}

// legacyTextTokens charges a JSON string, reporting whether the value was one.
func legacyTextTokens(raw json.RawMessage) (int64, bool) {
	text, ok := legacyTextOf(raw)
	if !ok {
		return 0, false
	}
	return legacyCharge(utf8.RuneCountInString(text)), true
}

// legacyEstimateText charges a field that holds text, and nothing for one that
// holds anything else.
func legacyEstimateText(raw json.RawMessage) int64 {
	tokens, _ := legacyTextTokens(raw)
	return tokens
}

// legacyCharge converts a character count into tokens, rounding up so that no text
// is free.
func legacyCharge(characters int) int64 {
	return int64((characters + legacyCharsPerToken - 1) / legacyCharsPerToken)
}

// legacyTextOf reads a JSON string, reporting whether the value was one. Characters
// are counted rather than bytes so a multi-byte script is not overcharged.
func legacyTextOf(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

// legacyJsonArray and legacyJsonObject decode a value of the shape the estimate expects,
// and nothing at all for any other shape.
func legacyJsonArray(raw json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

func legacyJsonObject(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

// legacyIntegerValue distinguishes null from an explicit integer.
func legacyIntegerValue(raw json.RawMessage) (int64, bool) {
	var value *int64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value == nil {
		return 0, false
	}
	return *value, true
}

// Native content uses the same text/media reservation units as OpenAI. Blob
// bytes are never mistaken for text tokens.
func legacyEstimateNative(raw json.RawMessage) int64 {
	if n, ok := legacyTextTokens(raw); ok {
		return n
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		var n int64
		for _, item := range items {
			n = legacyAddBounded(n, legacyEstimateNative(item))
		}
		return n
	}
	f := legacyJsonObject(raw)
	if f == nil {
		return 0
	}
	if f["inlineData"] != nil || f["fileData"] != nil || string(f["type"]) == `"image"` {
		return legacyImageTokens
	}
	var n int64
	for _, key := range []string{"text", "content", "parts", "contents", "systemInstruction", "functionResponse", "functionCall", "input", "output"} {
		if value := f[key]; value != nil {
			n = legacyAddBounded(n, legacyEstimateNative(value))
		}
	}
	return n
}

func legacyEstimateEmbeddingInput(raw json.RawMessage) int64 {
	if value, ok := legacyTextTokens(raw); ok {
		return value
	}
	total := int64(0)
	for _, item := range legacyJsonArray(raw) {
		var token uint32
		if json.Unmarshal(item, &token) == nil {
			total = legacyAddBounded(total, 1)
		} else {
			total = legacyAddBounded(total, legacyEstimateEmbeddingInput(item))
		}
	}
	return total
}
func legacyMultiplyBounded(a, b int64) int64 {
	if a > legacyMaxEstimate/b {
		return legacyMaxEstimate
	}
	return a * b
}

func legacyAddBounded(a, b int64) int64 {
	if a > legacyMaxEstimate-b {
		return legacyMaxEstimate
	}
	return a + b
}

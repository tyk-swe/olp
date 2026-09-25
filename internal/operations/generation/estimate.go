package generation

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

const (
	// CharsPerToken is the ratio text is charged at. Four characters per token
	// is a conservative portable approximation across connectors.
	CharsPerToken = 4
	// ImageTokens and MediaTokens are the flat charges for content whose size
	// on the wire says nothing about what a model will be billed for: an
	// inline image arrives as a megabyte of base64, and charging it by its
	// length would refuse requests no provider would have refused.
	ImageTokens = 1000
	MediaTokens = 2000
	// DefaultOutputTokens stands in for a caller that named no output bound.
	DefaultOutputTokens = 4096
	// MaxEstimate is the largest integer the limiter can store.
	MaxEstimate = 1<<53 - 1
)

// Total resolves the reservation: the prompt plus the largest allowed reply,
// multiplied by the candidates asked for. It is deliberately generous — the
// reservation is reconciled against real usage when the attempt ends.
func (e Estimate) Total() int64 {
	bound := int64(DefaultOutputTokens)
	if e.Output != nil {
		bound = *e.Output
	}
	return max(AddBounded(e.Input, MultiplyBounded(max(bound, 1), max(e.Candidates, 1))), 1)
}

// MultiplyBounded and AddBounded saturate at the largest integer the limiter
// accepts, so an absurd request is rejected as too large for the key's window
// rather than failing the reservation as malformed.
func MultiplyBounded(a, b int64) int64 {
	if a > MaxEstimate/b {
		return MaxEstimate
	}
	return a * b
}

func AddBounded(a, b int64) int64 {
	if a > MaxEstimate-b {
		return MaxEstimate
	}
	return a + b
}

// Integer distinguishes null from an explicit integer.
func Integer(raw json.RawMessage) (int64, bool) {
	var value *int64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value == nil {
		return 0, false
	}
	return *value, true
}

// JSONArray and JSONObject decode a value of the shape an estimate expects,
// and nothing at all for any other shape.
func JSONArray(raw json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

func JSONObject(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

// TextCharge converts a character count into tokens, rounding up so that no
// text is free.
func TextCharge(characters int) int64 {
	return int64((characters + CharsPerToken - 1) / CharsPerToken)
}

// TextTokens charges a JSON string, reporting whether the value was one.
// Characters are counted rather than bytes so a multi-byte script is not
// overcharged.
func TextTokens(raw json.RawMessage) (int64, bool) {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	return TextCharge(utf8.RuneCountInString(text)), true
}

// EstimateText charges a field that holds text, and nothing for one that
// holds anything else.
func EstimateText(raw json.RawMessage) int64 {
	tokens, _ := TextTokens(raw)
	return tokens
}

// EstimateItems charges the conversation one request carries: the messages of
// a chat request, or the input of a responses request, which may also be one
// plain string. A shape the estimator does not recognise is charged nothing
// rather than guessed at, because the reservation is reconciled against the
// usage the upstream reports as soon as the attempt ends.
func EstimateItems(raw json.RawMessage) int64 {
	if tokens, ok := TextTokens(raw); ok {
		return tokens
	}
	total := int64(0)
	for _, raw := range JSONArray(raw) {
		item := JSONObject(raw)
		if item == nil {
			continue
		}
		// A tool result carries what it returned in `output`.
		total = AddBounded(total, EstimateContent(item["content"]))
		total = AddBounded(total, EstimateContent(item["output"]))
		// The identifiers and arguments a message travels with are prompt text
		// like any other; `call_id` and `arguments` are one dialect's spelling
		// of another dialect's tool-call fields.
		for _, name := range [...]string{"name", "tool_call_id", "call_id", "arguments"} {
			total = AddBounded(total, EstimateText(item[name]))
		}
		for _, raw := range JSONArray(item["tool_calls"]) {
			call := JSONObject(raw)
			if function := JSONObject(call["function"]); function != nil {
				call = function
			}
			total = AddBounded(total, EstimateText(call["name"]))
			total = AddBounded(total, EstimateText(call["arguments"]))
		}
	}
	return total
}

// EstimateContent charges one message body: plain text, or the parts a
// multimodal message carries.
func EstimateContent(raw json.RawMessage) int64 {
	if tokens, ok := TextTokens(raw); ok {
		return tokens
	}
	total := int64(0)
	for _, raw := range JSONArray(raw) {
		part := JSONObject(raw)
		if part == nil {
			// A bare string among the parts is text.
			total = AddBounded(total, EstimateText(raw))
			continue
		}
		var kind string
		_ = json.Unmarshal(part["type"], &kind)
		switch kind {
		case "image_url", "input_image":
			total = AddBounded(total, ImageTokens)
		case "input_audio", "input_file", "file":
			total = AddBounded(total, MediaTokens)
		default:
			total = AddBounded(total, EstimateText(part["text"]))
			total = AddBounded(total, EstimateText(part["refusal"]))
		}
	}
	return total
}

// EstimateTools charges the tool catalogue a request carries: a schema the
// model has to read costs what any other prompt text costs.
func EstimateTools(raw json.RawMessage) int64 {
	total := int64(0)
	for _, raw := range JSONArray(raw) {
		tool := JSONObject(raw)
		// Some dialects nest the definition under `function`; others hold it flat.
		if function := JSONObject(tool["function"]); function != nil {
			tool = function
		}
		total = AddBounded(total, EstimateText(tool["name"]))
		total = AddBounded(total, EstimateText(tool["description"]))
		total = AddBounded(total, EstimateSchema(tool["parameters"]))
	}
	return total
}

// EstimateSchema charges a JSON schema as the document it is, with whatever
// formatting the caller happened to send removed.
func EstimateSchema(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return 0
	}
	return TextCharge(utf8.RuneCount(compact.Bytes()))
}

// EstimateNative charges a whole native subtree: text, or the flat media
// charge where the bytes say nothing about the billed cost. Blob bytes are
// never mistaken for text tokens.
func EstimateNative(raw json.RawMessage) int64 {
	if n, ok := TextTokens(raw); ok {
		return n
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		var n int64
		for _, item := range items {
			n = AddBounded(n, EstimateNative(item))
		}
		return n
	}
	f := JSONObject(raw)
	if f == nil {
		return 0
	}
	if f["inlineData"] != nil || f["fileData"] != nil || string(f["type"]) == `"image"` {
		return ImageTokens
	}
	var n int64
	for _, key := range []string{"text", "content", "parts", "contents", "systemInstruction", "functionResponse", "functionCall", "input", "output"} {
		if value := f[key]; value != nil {
			n = AddBounded(n, EstimateNative(value))
		}
	}
	return n
}

// EstimateEmbeddingInput charges tokenized or nested embedding input.
func EstimateEmbeddingInput(raw json.RawMessage) int64 {
	if value, ok := TextTokens(raw); ok {
		return value
	}
	total := int64(0)
	for _, item := range JSONArray(raw) {
		var token uint32
		if json.Unmarshal(item, &token) == nil {
			total = AddBounded(total, 1)
		} else {
			total = AddBounded(total, EstimateEmbeddingInput(item))
		}
	}
	return total
}

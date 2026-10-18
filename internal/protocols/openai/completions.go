package openai

import (
	"encoding/json"
	"fmt"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// Legacy completions have their own prompt and result grammar. Sharing the
// generation operation never makes a chat-only profile a native text endpoint.
func validateCompletions(r *wire.Request, fields map[string]json.RawMessage) error {
	for _, name := range []string{"store", "background", "conversation", "previous_response_id", "input_file_id", "file_id"} {
		if _, present := fields[name]; present {
			return invalid(name, "Legacy completions do not accept retained state or resource references.")
		}
	}

	if !validCompletionPrompt(fields["prompt"]) {
		return invalid("prompt", "prompt must be text, text arrays, token IDs, or arrays of token IDs.")
	}
	for _, name := range []string{"max_tokens", "n", "best_of", "seed", "logprobs"} {
		if err := integerField(fields, name, name == "n" || name == "best_of"); err != nil {
			return err
		}
		if name == "logprobs" && len(fields[name]) > 0 && !isNull(fields[name]) {
			if n, ok := int64Field(fields, name); !ok || n > 5 {
				return invalid(name, "logprobs must be between 0 and 5.")
			}
		}
	}
	if n, ok := int64Field(fields, "max_tokens"); !ok && len(fields["max_tokens"]) > 0 && !isNull(fields["max_tokens"]) || ok && n < 0 {
		return invalid("max_tokens", "max_tokens must be a non-negative integer.")
	}
	for _, name := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
		if err := numberField(fields, name); err != nil {
			return err
		}
	}
	if value, exists := fields["echo"]; exists && !isNull(value) {
		var b bool
		if json.Unmarshal(value, &b) != nil {
			return invalid("echo", "echo must be a boolean.")
		}
	}
	// No first-release native profile certifies fill-in-the-middle suffixes.
	if value, exists := fields["suffix"]; exists && !isNull(value) {
		return invalid("suffix", "The selected completions contract does not certify suffix.")
	}
	if raw, present := fields["stream_options"]; present && !isNull(raw) {
		options, err := object(raw)
		if err != nil {
			return invalid("stream_options", "stream_options must be an object.")
		}
		if raw, present := options["include_usage"]; present && !isNull(raw) {
			if json.Unmarshal(raw, &r.IncludeUsage) != nil {
				return invalid("stream_options.include_usage", "include_usage must be a boolean.")
			}
		}
	}
	return nil
}

func validCompletionPrompt(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil && !isNull(raw) {
		return true
	}
	if validEmbeddingInput(raw) && !isNull(raw) {
		return true
	}
	return false
}

// DecodeCompletions preserves native extension fields and lexical values while
// binding only model identity. Every candidate must have terminal evidence.
func DecodeCompletions(body []byte, route string) (*wire.Completion, error) {
	source, err := liftResult(wire.Completions, body)
	if err != nil {
		return nil, err
	}
	fields := source.Source().Fields()
	if fields == nil {
		return nil, &wire.ProtocolError{Detail: "completion is not an object"}
	}
	if raw, present := fields["error"]; present && !isNull(raw) {
		if e := errorObject(raw); e != nil {
			return nil, e
		}
	}
	if kind, ok := stringField(fields, "object"); ok && kind != "text_completion" {
		return nil, &wire.ProtocolError{Detail: "unexpected completion object"}
	}
	choices, ok := arrayField(fields, "choices")
	if !ok || len(choices) == 0 {
		return nil, &wire.ProtocolError{Detail: "completion has no choices"}
	}
	c := &wire.Completion{Native: source}
	c.UpstreamID, _ = stringField(fields, "id")
	c.ProviderModel, _ = stringField(fields, "model")
	seen := map[int64]bool{}
	for i, raw := range choices {
		choice, e := object(raw)
		if e != nil {
			return nil, &wire.ProtocolError{Detail: "invalid completion choice"}
		}
		index, ok := int64Field(choice, "index")
		if !ok || seen[index] {
			return nil, &wire.ProtocolError{Detail: "completion indices must be unique non-negative integers"}
		}
		seen[index] = true
		text, ok := stringField(choice, "text")
		if !ok {
			return nil, &wire.ProtocolError{Detail: "completion text must be a string"}
		}
		reason, ok := stringField(choice, "finish_reason")
		if !ok || reason == "" {
			return nil, &wire.ProtocolError{Detail: "completion lacks a terminal finish reason"}
		}
		if i == 0 {
			c.OutputText = text
			c.FinishReason = reason
		}
	}
	c.Usage, err = chatUsage(fields, true)
	if err != nil {
		return nil, err
	}
	model, _ := json.Marshal(route)
	doc, err := oif.Apply(source.Source(), []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published route model"}})
	if err != nil {
		return nil, fmt.Errorf("completion model binding: %w", err)
	}
	c.Body = doc.Bytes()
	return c, nil
}

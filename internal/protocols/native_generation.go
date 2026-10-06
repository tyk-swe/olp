package protocols

import (
	"encoding/json"
	"io"
	"math"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

// Native generation dialects are vendor generation APIs a client speaks as
// they are, at /native/{dialect}/models/{route}. OLP validates their envelope,
// binds the route's model, meters the result and forwards it unchanged; it
// never translates them to or from another dialect.
var nativeGenerationFamilies = map[string]openai.Family{
	// https://docs.mistral.ai/api/endpoint/fim
	"mistral-fim": openai.FamilyMistralFIM,
	// https://docs.cohere.com/reference/chat
	"cohere-chat-v2": openai.FamilyCohereChat,
}

// NativeGenerationFamily is the generation family of a native dialect.
func NativeGenerationFamily(dialect string) (openai.Family, bool) {
	family, ok := nativeGenerationFamilies[dialect]
	return family, ok
}

// NativeGenerationProbe is the smallest request of a native dialect that
// proves a model generates: a few tokens of output.
func NativeGenerationProbe(family openai.Family) map[string]any {
	if family == openai.FamilyMistralFIM {
		return map[string]any{"model": "certification", "prompt": "def add(a, b):\n    return ", "suffix": "\n", "max_tokens": 16}
	}
	return map[string]any{"model": "certification", "messages": []map[string]string{{"role": "user", "content": "Reply with OK."}}, "max_tokens": 16}
}

// parseNativeGeneration validates the envelope of a native generation request
// for route: its model, if it names one, is the route, and its delivery and
// conversation are well formed. The rest belongs to the vendor, who refuses
// what it does not accept.
func parseNativeGeneration(family openai.Family, doc oif.Document, route string) (*openai.Request, error) {
	f := doc.FieldsWithout("messages")
	if f == nil {
		return nil, requestError("", "expected a JSON object")
	}
	if !openai.RouteSlug.MatchString(route) {
		return nil, requestError("model", "model must name a published route slug")
	}
	if present(f["model"]) && str(f["model"]) != route {
		return nil, requestError("model", "model must be absent or name the route in the request path")
	}
	stream := false
	if present(f["stream"]) {
		if err := json.Unmarshal(f["stream"], &stream); err != nil {
			return nil, requestError("stream", "stream must be a boolean")
		}
	}
	switch family {
	case openai.FamilyMistralFIM:
		if prompt, ok := f["prompt"]; !ok || json.Unmarshal(prompt, new(string)) != nil {
			return nil, requestError("prompt", "prompt must be a string")
		}
		if present(f["suffix"]) && json.Unmarshal(f["suffix"], new(string)) != nil {
			return nil, requestError("suffix", "suffix must be a string")
		}
		for _, name := range []string{"max_tokens", "min_tokens"} {
			if present(f[name]) {
				if n, ok := count(f[name]); !ok || n < 0 {
					return nil, requestError(name, name+" must be a non-negative integer")
				}
			}
		}
	case openai.FamilyCohereChat:
		messages := member(doc.Root(), "messages").Elements()
		if len(messages) == 0 {
			return nil, requestError("messages", "messages must be a non-empty array")
		}
		for _, message := range messages {
			if message.Kind() != oif.Object {
				return nil, requestError("messages", "each message must be an object")
			}
			switch chars(member(message, "role")) {
			case "user", "assistant", "system", "tool":
			default:
				return nil, requestError("messages.role", "Cohere messages require user, assistant, system or tool roles")
			}
		}
	default:
		return nil, requestError("", "unknown native generation dialect")
	}
	return openai.NewSourceEnvelope(family, route, stream, doc), nil
}

// decodeCohereChat meters a Cohere Chat v2 result. Cohere bills the
// billed_units it reports, which leave out the tokens of its own preamble.
func decodeCohereChat(body []byte) (*openai.Completion, error) {
	f, err := object(body)
	if err != nil {
		return nil, protocolError("chat result is not a JSON object")
	}
	message, err := object(f["message"])
	if err != nil {
		return nil, protocolError("chat result has no message")
	}
	c := &openai.Completion{Body: body, UpstreamID: str(f["id"]), FinishReason: cohereFinish(str(f["finish_reason"]))}
	if c.FinishReason == "" {
		return nil, protocolError("chat result has no finish reason")
	}
	for _, part := range arr(message["content"]) {
		if content, err := object(part); err == nil && str(content["type"]) == "text" {
			c.OutputText += str(content["text"])
		}
	}
	for _, call := range arr(message["tool_calls"]) {
		tool, err := object(call)
		if err != nil {
			return nil, protocolError("invalid tool call")
		}
		function, _ := object(tool["function"])
		c.ToolCalls = append(c.ToolCalls, openai.ToolCall{ID: str(tool["id"]), Name: str(function["name"]), Arguments: str(function["arguments"])})
	}
	if c.Usage, err = cohereUsage(f["usage"]); err != nil {
		return nil, err
	}
	return c, nil
}

// cohereFinish is the OpenAI finish reason of a Cohere one.
func cohereFinish(reason string) string {
	switch reason {
	case "COMPLETE", "STOP_SEQUENCE":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "TOOL_CALL":
		return "tool_calls"
	case "ERROR", "TIMEOUT":
		return "error"
	}
	return ""
}

func cohereUsage(raw json.RawMessage) (*openai.Usage, error) {
	if !present(raw) {
		return nil, nil
	}
	usage, err := object(raw)
	if err != nil {
		return nil, protocolError("invalid usage")
	}
	billed, err := optionalObject(usage["billed_units"])
	if err != nil || billed == nil {
		return nil, protocolError("usage has no billed units")
	}
	// Cohere types its counts as numbers that may carry a fraction.
	tokens := func(name string) (int64, bool) {
		var n float64
		if json.Unmarshal(billed[name], &n) != nil || n < 0 || math.IsInf(n, 0) || n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	}
	input, inputOK := tokens("input_tokens")
	output, outputOK := tokens("output_tokens")
	if !inputOK || !outputOK {
		return nil, protocolError("invalid billed units")
	}
	return &openai.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output}, nil
}

// streamCohereEvents forwards a Cohere Chat v2 stream's events unchanged and
// meters it. message-end is its terminal event, carrying the finish reason and
// usage; a stream that ends before it is truncated.
func streamCohereEvents(r io.Reader, limit int, emit openai.Emit, observe func(oif.Event) error) (*openai.Completion, error) {
	c := &openai.Completion{}
	done := false
	sequence := uint64(0)
	err := sse.Decode(r, limit, func(frame sse.Frame) error {
		if done {
			return protocolError("event after message-end")
		}
		event, err := openai.LiftSSE(openai.FamilyCohereChat, frame, sequence, limit)
		if err != nil {
			return err
		}
		sequence++
		if observe != nil {
			if err := observe(event); err != nil {
				return err
			}
		}
		f, err := object([]byte(frame.Data))
		if err != nil {
			return protocolError("event is not a JSON object")
		}
		kind := str(f["type"])
		if frame.Event != nil && *frame.Event != kind {
			return protocolError("event name disagrees with its type")
		}
		delta, _ := optionalObject(f["delta"])
		switch kind {
		case "message-start":
			c.UpstreamID = str(f["id"])
		case "message-end":
			if present(delta["error"]) {
				return &openai.UpstreamError{Type: "stream_error", Message: "Cohere ended the stream with an error"}
			}
			if c.FinishReason = cohereFinish(str(delta["finish_reason"])); c.FinishReason == "" {
				return protocolError("message-end has no finish reason")
			}
			if c.Usage, err = cohereUsage(delta["usage"]); err != nil {
				return err
			}
			done = true
		case "content-start", "content-delta", "content-end", "tool-plan-delta", "tool-call-start", "tool-call-delta", "tool-call-end", "citation-start", "citation-end", "debug":
		default:
			return protocolError("unknown event " + kind)
		}
		return emit(frame.Encode())
	})
	if err != nil {
		return c, streamError(err)
	}
	if !done {
		return c, &openai.ProtocolError{Detail: "stream ended before message-end", Truncated: true}
	}
	return c, nil
}

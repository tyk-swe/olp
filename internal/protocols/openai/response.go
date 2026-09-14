package openai

import (
	"encoding/json"
	"strings"
)

// Usage is the token accounting observed on one generation.
type Usage struct {
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	TotalTokens       int64  `json:"total_tokens"`
	CachedInputTokens *int64 `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   *int64 `json:"reasoning_tokens,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Completion summarises an observed generation, which may be partial on stream
// failure. Body is the rewritten unary document and is nil for streams.
type Completion struct {
	Body          []byte
	UpstreamID    string
	ProviderModel string
	FinishReason  string
	OutputText    string
	Refusal       string
	ToolCalls     []ToolCall
	Usage         *Usage
}

// ProtocolError reports an upstream document or stream that violates the
// OpenAI protocol. Truncated marks streams that ended before their terminal event.
type ProtocolError struct {
	Detail    string
	Truncated bool
}

func (e *ProtocolError) Error() string { return "upstream protocol violation: " + e.Detail }

// UpstreamError is an error object the upstream returned in place of a result.
type UpstreamError struct {
	Type    string
	Code    string
	Message string
}

func (e *UpstreamError) Error() string { return "upstream error: " + e.Message }

// ParseErrorBody extracts an OpenAI error envelope, tolerating other shapes.
func ParseErrorBody(body []byte) *UpstreamError {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Error) == 0 {
		return nil
	}
	return errorObject(envelope.Error)
}

func errorObject(raw json.RawMessage) *UpstreamError {
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return &UpstreamError{Message: message}
	}
	var detail struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &detail) != nil {
		return nil
	}
	return &UpstreamError{Type: detail.Type, Code: strings.Trim(string(detail.Code), `"`), Message: detail.Message}
}

// DecodeChat validates a unary chat completion and rewrites its model to the route.
func DecodeChat(body []byte, route string) (*Completion, error) {
	fields, err := object(body)
	if err != nil {
		return nil, &ProtocolError{Detail: "chat completion is not a JSON object"}
	}
	if raw, present := fields["error"]; present && !isNull(raw) {
		if upstream := errorObject(raw); upstream != nil {
			return nil, upstream
		}
	}
	if kind, ok := stringField(fields, "object"); ok && kind != "chat.completion" {
		return nil, &ProtocolError{Detail: "unexpected object " + kind}
	}
	choices, ok := arrayField(fields, "choices")
	if !ok || len(choices) == 0 {
		return nil, &ProtocolError{Detail: "chat completion has no choices"}
	}
	c := &Completion{}
	c.UpstreamID, _ = stringField(fields, "id")
	c.ProviderModel, _ = stringField(fields, "model")
	choice, err := object(choices[0])
	if err != nil {
		return nil, &ProtocolError{Detail: "choice is not an object"}
	}
	c.FinishReason, _ = stringField(choice, "finish_reason")
	if raw, present := choice["message"]; present && !isNull(raw) {
		message, err := object(raw)
		if err != nil {
			return nil, &ProtocolError{Detail: "message is not an object"}
		}
		c.OutputText, _ = stringField(message, "content")
		c.Refusal, _ = stringField(message, "refusal")
		if calls, ok := arrayField(message, "tool_calls"); ok {
			for _, raw := range calls {
				call, err := object(raw)
				if err != nil {
					return nil, &ProtocolError{Detail: "tool call is not an object"}
				}
				c.ToolCalls = append(c.ToolCalls, chatToolCall(call))
			}
		}
	}
	if c.Usage, err = chatUsage(fields); err != nil {
		return nil, err
	}
	if fields["model"], err = json.Marshal(route); err != nil {
		return nil, err
	}
	if c.Body, err = json.Marshal(fields); err != nil {
		return nil, err
	}
	return c, nil
}

func chatToolCall(call map[string]json.RawMessage) ToolCall {
	var tc ToolCall
	tc.ID, _ = stringField(call, "id")
	if raw, present := call["function"]; present {
		if function, err := object(raw); err == nil {
			tc.Name, _ = stringField(function, "name")
			tc.Arguments, _ = stringField(function, "arguments")
		}
	}
	return tc
}

func chatUsage(fields map[string]json.RawMessage) (*Usage, error) {
	raw, present := fields["usage"]
	if !present || isNull(raw) {
		return nil, nil
	}
	usage, err := object(raw)
	if err != nil {
		return nil, &ProtocolError{Detail: "usage is not an object"}
	}
	u := &Usage{}
	var ok bool
	if u.InputTokens, ok = int64Field(usage, "prompt_tokens"); !ok {
		return nil, &ProtocolError{Detail: "usage.prompt_tokens is not a non-negative integer"}
	}
	if u.OutputTokens, ok = int64Field(usage, "completion_tokens"); !ok {
		return nil, &ProtocolError{Detail: "usage.completion_tokens is not a non-negative integer"}
	}
	if u.TotalTokens, ok = int64Field(usage, "total_tokens"); !ok {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	u.CachedInputTokens = nestedCount(usage, "prompt_tokens_details", "cached_tokens")
	u.ReasoningTokens = nestedCount(usage, "completion_tokens_details", "reasoning_tokens")
	return u, nil
}

func nestedCount(fields map[string]json.RawMessage, parent, name string) *int64 {
	raw, present := fields[parent]
	if !present || isNull(raw) {
		return nil
	}
	details, err := object(raw)
	if err != nil {
		return nil
	}
	if n, ok := int64Field(details, name); ok {
		return &n
	}
	return nil
}

// DecodeResponse validates a unary Responses API document and rewrites its model.
func DecodeResponse(body []byte, route string) (*Completion, error) {
	fields, err := object(body)
	if err != nil {
		return nil, &ProtocolError{Detail: "response is not a JSON object"}
	}
	if err := terminalResponseError(fields); err != nil {
		return nil, err
	}
	c, err := responseSummary(fields)
	if err != nil {
		return nil, err
	}
	if fields["model"], err = json.Marshal(route); err != nil {
		return nil, err
	}
	if c.Body, err = json.Marshal(fields); err != nil {
		return nil, err
	}
	return c, nil
}

// terminalResponseError rejects failures and stateful, non-terminal responses
// even when an upstream omits its error object. Sparse compatible responses may
// omit status, but an explicitly supplied status must describe a terminal result.
func terminalResponseError(fields map[string]json.RawMessage) error {
	if raw, present := fields["error"]; present && !isNull(raw) {
		if upstream := errorObject(raw); upstream != nil {
			return upstream
		}
		return &ProtocolError{Detail: "response has an invalid error object"}
	}
	if _, present := fields["status"]; !present {
		return nil
	}
	status, _ := stringField(fields, "status")
	switch status {
	case "completed", "incomplete":
		return nil
	case "failed":
		return &UpstreamError{Message: "upstream reported a failed response"}
	default:
		return &ProtocolError{Detail: "response is not terminal"}
	}
}

func responseSummary(fields map[string]json.RawMessage) (*Completion, error) {
	if kind, ok := stringField(fields, "object"); ok && kind != "response" {
		return nil, &ProtocolError{Detail: "unexpected object " + kind}
	}
	output, ok := arrayField(fields, "output")
	if !ok {
		return nil, &ProtocolError{Detail: "response has no output array"}
	}
	c := &Completion{}
	c.UpstreamID, _ = stringField(fields, "id")
	c.ProviderModel, _ = stringField(fields, "model")
	var text strings.Builder
	for _, raw := range output {
		item, err := object(raw)
		if err != nil {
			return nil, &ProtocolError{Detail: "output item is not an object"}
		}
		kind, _ := stringField(item, "type")
		switch kind {
		case "message":
			parts, _ := arrayField(item, "content")
			for _, raw := range parts {
				part, err := object(raw)
				if err != nil {
					continue
				}
				switch partKind, _ := stringField(part, "type"); partKind {
				case "output_text":
					value, _ := stringField(part, "text")
					text.WriteString(value)
				case "refusal":
					c.Refusal, _ = stringField(part, "refusal")
				}
			}
		case "function_call":
			var tc ToolCall
			tc.ID, _ = stringField(item, "call_id")
			tc.Name, _ = stringField(item, "name")
			tc.Arguments, _ = stringField(item, "arguments")
			c.ToolCalls = append(c.ToolCalls, tc)
		}
	}
	c.OutputText = text.String()
	status, _ := stringField(fields, "status")
	switch {
	case len(c.ToolCalls) > 0:
		c.FinishReason = "tool_calls"
	case status == "incomplete":
		c.FinishReason = "length"
		if raw, present := fields["incomplete_details"]; present {
			if details, err := object(raw); err == nil {
				if reason, ok := stringField(details, "reason"); ok && reason == "content_filter" {
					c.FinishReason = "content_filter"
				}
			}
		}
	case status == "completed" || status == "":
		c.FinishReason = "stop"
	default:
		c.FinishReason = status
	}
	var err error
	if c.Usage, err = responseUsage(fields); err != nil {
		return nil, err
	}
	return c, nil
}

func responseUsage(fields map[string]json.RawMessage) (*Usage, error) {
	raw, present := fields["usage"]
	if !present || isNull(raw) {
		return nil, nil
	}
	usage, err := object(raw)
	if err != nil {
		return nil, &ProtocolError{Detail: "usage is not an object"}
	}
	u := &Usage{}
	var ok bool
	if u.InputTokens, ok = int64Field(usage, "input_tokens"); !ok {
		return nil, &ProtocolError{Detail: "usage.input_tokens is not a non-negative integer"}
	}
	if u.OutputTokens, ok = int64Field(usage, "output_tokens"); !ok {
		return nil, &ProtocolError{Detail: "usage.output_tokens is not a non-negative integer"}
	}
	if u.TotalTokens, ok = int64Field(usage, "total_tokens"); !ok {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	u.CachedInputTokens = nestedCount(usage, "input_tokens_details", "cached_tokens")
	u.ReasoningTokens = nestedCount(usage, "output_tokens_details", "reasoning_tokens")
	return u, nil
}

func rewriteModel(fields map[string]json.RawMessage, route string) error {
	if _, present := fields["model"]; !present {
		return nil
	}
	raw, err := json.Marshal(route)
	if err != nil {
		return err
	}
	fields["model"] = raw
	return nil
}

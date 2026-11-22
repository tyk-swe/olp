package openai

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

// DecodeResponse validates a unary Responses API document and rewrites its model.
func DecodeResponse(body []byte, route string) (*wire.Completion, error) {
	return decodeResponse(body, route, false)
}

// DecodeBackgroundResponse also accepts the queued and in-progress objects
// returned by a provider after accepting an explicitly background request.
func DecodeBackgroundResponse(body []byte, route string) (*wire.Completion, error) {
	return decodeResponse(body, route, true)
}

func decodeResponse(body []byte, route string, background bool) (*wire.Completion, error) {
	source, err := liftResult(wire.Responses, body)
	if err != nil {
		return nil, err
	}
	fields := source.Source().Fields()
	if fields == nil {
		return nil, &wire.ProtocolError{Detail: "response is not a JSON object"}
	}
	status, _ := stringField(fields, "status")
	pending := background && (status == "queued" || status == "in_progress")
	if !pending {
		if err := terminalResponseError(fields); err != nil {
			return nil, err
		}
	} else if raw, present := fields["error"]; present && !isNull(raw) {
		return nil, &wire.ProtocolError{Detail: "pending response contains an error"}
	}
	c, err := responseSummary(fields)
	if err != nil {
		return nil, err
	}
	c.UsageComplete = !pending && c.Usage != nil
	c.Native = source
	if pending {
		c.Native, _ = oif.NewResult(source.Descriptor(), source.Source(), oif.Pending)
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
		return &wire.ProtocolError{Detail: "response has an invalid error object"}
	}
	if _, present := fields["status"]; !present {
		return nil
	}
	status, _ := stringField(fields, "status")
	switch status {
	case "completed", "incomplete":
		return nil
	case "failed":
		return &wire.UpstreamError{Message: "upstream reported a failed response"}
	default:
		return &wire.ProtocolError{Detail: "response is not terminal"}
	}
}

func responseSummary(fields map[string]json.RawMessage) (*wire.Completion, error) {
	usage, err := responseUsage(fields)
	if err != nil {
		return nil, err
	}
	return responseSummaryWithUsage(fields, usage)
}

func responseSummaryWithUsage(fields map[string]json.RawMessage, usage *wire.Usage) (*wire.Completion, error) {
	if kind, ok := stringField(fields, "object"); ok && kind != "response" {
		return nil, &wire.ProtocolError{Detail: "unexpected object " + kind}
	}
	output, ok := arrayField(fields, "output")
	if !ok {
		return nil, &wire.ProtocolError{Detail: "response has no output array"}
	}
	c := &wire.Completion{Usage: usage}
	c.UpstreamID, _ = stringField(fields, "id")
	c.ProviderModel, _ = stringField(fields, "model")
	var text strings.Builder
	for _, raw := range output {
		item, err := object(raw)
		if err != nil {
			return nil, &wire.ProtocolError{Detail: "output item is not an object"}
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
			var tc wire.ToolCall
			tc.ID, _ = stringField(item, "call_id")
			tc.Name, _ = stringField(item, "name")
			tc.Arguments, _ = stringField(item, "arguments")
			c.ToolCalls = append(c.ToolCalls, tc)
		}
	}
	if usage != nil {
		usage.ToolCalls = responseToolCalls(fields["output"])
	}
	c.OutputText = text.String()
	status, _ := stringField(fields, "status")
	switch {
	case status == "incomplete":
		c.FinishReason = "length"
		if raw, present := fields["incomplete_details"]; present {
			if details, err := object(raw); err == nil {
				if reason, ok := stringField(details, "reason"); ok && reason == "content_filter" {
					c.FinishReason = "content_filter"
				}
			}
		}
	case len(c.ToolCalls) > 0:
		c.FinishReason = "tool_calls"
	case status == "completed" || status == "":
		c.FinishReason = "stop"
	default:
		c.FinishReason = status
	}
	return c, nil
}

// responseToolCalls counts the server-side tools a Responses reply billed:
// only completed web_search_call and file_search_call items are invocations.
// responseToolCallsDocument counts the same invocations on an already parsed
// output array, touching only the fields that name a call — the generated
// text itself is never copied.
func responseToolCallsDocument(output oif.Value) map[string]int64 {
	var calls map[string]int64
	for _, item := range output.Elements() {
		kind, ok := item.Lookup("type")
		if !ok || kind.Kind() != oif.String {
			continue
		}
		name, ok := kind.Chars()
		if !ok || name != "web_search_call" && name != "file_search_call" {
			continue
		}
		status, ok := item.Lookup("status")
		if !ok || status.Kind() != oif.String {
			continue
		}
		done, ok := status.Chars()
		if !ok || done != "completed" {
			continue
		}
		if calls == nil {
			calls = map[string]int64{}
		}
		switch name {
		case "web_search_call":
			calls["web_search"]++
		case "file_search_call":
			calls["file_search"]++
		}
	}
	return calls
}

func responseToolCalls(raw json.RawMessage) map[string]int64 {
	if raw == nil || isNull(raw) ||
		!bytes.Contains(raw, []byte("web_search_call")) && !bytes.Contains(raw, []byte("file_search_call")) {
		return nil
	}
	var items []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	var calls map[string]int64
	for _, item := range items {
		var tool string
		switch item.Type {
		case "web_search_call":
			tool = "web_search"
		case "file_search_call":
			tool = "file_search"
		default:
			continue
		}
		if item.Status != "completed" {
			continue
		}
		if calls == nil {
			calls = map[string]int64{}
		}
		calls[tool]++
	}
	return calls
}

func responseUsage(fields map[string]json.RawMessage) (*wire.Usage, error) {
	raw, present := fields["usage"]
	if !present || isNull(raw) {
		return nil, nil
	}
	usage, err := object(raw)
	if err != nil {
		return nil, &wire.ProtocolError{Detail: "usage is not an object"}
	}
	u := &wire.Usage{}
	var ok bool
	if u.InputTokens, ok = int64Field(usage, "input_tokens"); !ok {
		return nil, &wire.ProtocolError{Detail: "usage.input_tokens is not a non-negative integer"}
	}
	if u.OutputTokens, ok = int64Field(usage, "output_tokens"); !ok {
		return nil, &wire.ProtocolError{Detail: "usage.output_tokens is not a non-negative integer"}
	}
	if raw, present := usage["total_tokens"]; present && !isNull(raw) {
		if u.TotalTokens, ok = int64Field(usage, "total_tokens"); !ok {
			return nil, &wire.ProtocolError{Detail: "usage.total_tokens is not a non-negative integer"}
		}
	} else {
		if u.InputTokens > math.MaxInt64-u.OutputTokens {
			return nil, &wire.ProtocolError{Detail: "usage token total is out of range"}
		}
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if u.CachedInputTokens, err = responseDetailCount(usage, "input_tokens_details", "cached_tokens"); err != nil {
		return nil, err
	}
	if u.CachedInputTokens != nil && *u.CachedInputTokens > u.InputTokens {
		return nil, &wire.ProtocolError{Detail: "usage cached input exceeds input tokens"}
	}
	if u.ReasoningTokens, err = responseDetailCount(usage, "output_tokens_details", "reasoning_tokens"); err != nil {
		return nil, err
	}
	if u.ReasoningTokens != nil && *u.ReasoningTokens > u.OutputTokens {
		return nil, &wire.ProtocolError{Detail: "usage reasoning tokens exceed output tokens"}
	}
	return u, nil
}

func responseDetailCount(usage map[string]json.RawMessage, parent, name string) (*int64, error) {
	raw, present := usage[parent]
	if !present || isNull(raw) {
		return nil, nil
	}
	details, err := object(raw)
	if err != nil {
		return nil, &wire.ProtocolError{Detail: "usage." + parent + " is not an object"}
	}
	if raw, present := details[name]; !present || isNull(raw) {
		return nil, nil
	}
	n, ok := int64Field(details, name)
	if !ok {
		return nil, &wire.ProtocolError{Detail: "usage." + parent + "." + name + " is not a non-negative integer"}
	}
	return &n, nil
}

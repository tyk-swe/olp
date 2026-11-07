package gateway

import (
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func codeTranslateGrok(body []byte, r *http.Request, model string) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.URL.RawQuery != "" || len(r.Trailer) != 0 || r.Header.Get("X-Codex-Turn-State") != "" || r.Header.Get("OpenAI-Beta") != "" {
		return codePayload{}, refused
	}
	for header := range r.Header {
		if strings.HasPrefix(strings.ToLower(header), "x-grok-") || strings.EqualFold(header, "X-Xai-Token-Auth") || strings.EqualFold(header, "X-Authenticateresponse") {
			return codePayload{}, refused
		}
	}
	request, err := protocols.ParseForModel(wire.Responses, body, model)
	if err != nil {
		return codePayload{}, refused
	}
	root := request.OIF().Document().Root()
	for _, field := range root.Members() {
		if !slices.Contains([]string{"model", "input", "instructions", "stream", "store", "tools", "tool_choice", "parallel_tool_calls", "max_output_tokens", "temperature", "top_p", "text", "reasoning", "metadata", "prompt_cache_key", "safety_identifier", "include", "client_metadata"}, field.Name) {
			return codePayload{}, refused
		}
	}
	stored, _ := root.Lookup("store")
	if stored.Kind() != oif.Absent && stored.Kind() != oif.Null && string(stored.Raw()) != "false" {
		return codePayload{}, refused
	}
	include, _ := root.Lookup("include")
	if include.Kind() != oif.Absent && include.Kind() != oif.Null {
		if include.Kind() != oif.Array {
			return codePayload{}, refused
		}
		for _, item := range include.Elements() {
			if stringValue(item) != "reasoning.encrypted_content" {
				return codePayload{}, refused
			}
		}
	}
	metadata, _ := root.Lookup("client_metadata")
	if metadata.Kind() != oif.Absent && metadata.Kind() != oif.Null {
		if metadata.Kind() != oif.Object || len(metadata.Members()) > 64 {
			return codePayload{}, refused
		}
		for _, field := range metadata.Members() {
			if field.Value.Kind() != oif.String || len(field.Value.Raw()) > 16384 {
				return codePayload{}, refused
			}
		}
	}
	input, _ := root.Lookup("input")
	if input.Kind() != oif.String && input.Kind() != oif.Array {
		return codePayload{}, refused
	}
	for _, item := range input.Elements() {
		kind, _ := item.Lookup("type")
		switch stringValue(kind) {
		case "", "message":
			role, _ := item.Lookup("role")
			if !slices.Contains([]string{"user", "assistant", "system", "developer"}, stringValue(role)) || !grokTextContent(item, "content") {
				return codePayload{}, refused
			}
		case "function_call":
			if !grokString(item, "call_id") || !grokString(item, "name") || !grokString(item, "arguments") {
				return codePayload{}, refused
			}
		case "function_call_output":
			if !grokString(item, "call_id") || !grokString(item, "output") {
				return codePayload{}, refused
			}
		default:
			return codePayload{}, refused
		}
	}
	tools, _ := root.Lookup("tools")
	for _, tool := range tools.Elements() {
		kind, _ := tool.Lookup("type")
		if stringValue(kind) != "function" {
			return codePayload{}, refused
		}
	}
	return codePayload{body: body, stream: request.Stream, target: codeadapter.TargetGrok}, nil
}

func grokString(item oif.Value, field string) bool {
	value, _ := item.Lookup(field)
	return value.Kind() == oif.String
}

func grokTextContent(item oif.Value, field string) bool {
	content, _ := item.Lookup(field)
	if content.Kind() == oif.String {
		return true
	}
	if content.Kind() != oif.Array {
		return false
	}
	for _, part := range content.Elements() {
		kind, _ := part.Lookup("type")
		if !slices.Contains([]string{"input_text", "output_text"}, stringValue(kind)) || !grokString(part, "text") {
			return false
		}
	}
	return true
}

func codeTranslateGrokMessages(body []byte, r *http.Request, model string, safetyMapping bool) (codePayload, error) {
	request, safety, err := codeParseMessages(body, r, model, safetyMapping)
	if err != nil {
		return codePayload{}, err
	}
	target, err := codeEncodeResponses(request)
	if err != nil {
		return codePayload{}, err
	}
	prepared, err := target.OIF().WithChanges(codeSafetyIdentifierChanges(safety)...)
	if err != nil {
		return codePayload{}, codemode.Refuse(422, "code_translation_unsupported_semantics")
	}
	clean := r.Clone(r.Context())
	clean.URL.RawQuery = ""
	payload, err := codeTranslateGrok(prepared.Document().Bytes(), clean, model)
	if err != nil {
		return codePayload{}, err
	}
	return codePayload{body: payload.body, stream: request.Stream, translated: true, target: codeadapter.TargetGrok}, nil
}

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
	if !codeResponsesInput(input) {
		return codePayload{}, refused
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

func codeTranslateGrokChat(body []byte, r *http.Request, model string) (codePayload, error) {
	request, err := codeParseChat(body, r, model)
	if err != nil {
		return codePayload{}, err
	}
	effort, _ := request.OIF().Document().Root().Lookup("reasoning_effort")
	if effort.Kind() != oif.Absent && effort.Kind() != oif.Null {
		return codePayload{}, codemode.Refuse(422, "code_translation_unsupported_semantics")
	}
	encoded, encodeErr := codeEncodeResponses(request)
	if encodeErr != nil {
		return codePayload{}, codemode.Refuse(422, "code_translation_unsupported_semantics")
	}
	payload, payloadErr := codeTranslateGrok(encoded.OIF().Document().Bytes(), r, model)
	if payloadErr != nil {
		return codePayload{}, payloadErr
	}
	return codePayload{body: payload.body, stream: request.Stream, translated: true, includeUsage: request.IncludeUsage, target: codeadapter.TargetGrok}, nil
}

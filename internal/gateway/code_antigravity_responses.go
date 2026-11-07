package gateway

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func codePrepareAntigravityResponses(request *wire.Request, r *http.Request, compatibility bool) (*wire.Request, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.Header.Get("X-Codex-Turn-State") != "" || r.Header.Get("OpenAI-Beta") != "" {
		return nil, refused
	}
	root := request.OIF().Document().Root()
	for _, field := range root.Members() {
		if !slices.Contains([]string{"model", "input", "instructions", "stream", "tools", "tool_choice", "max_output_tokens", "temperature", "top_p", "include"}, field.Name) && (!compatibility || !slices.Contains([]string{"store", "parallel_tool_calls", "prompt_cache_key", "client_metadata", "reasoning"}, field.Name)) {
			return nil, refused
		}
	}
	include, _ := root.Lookup("include")
	if include.Kind() != oif.Absent {
		if include.Kind() != oif.Array || (!compatibility && len(include.Elements()) != 0) {
			return nil, refused
		}
		for _, value := range include.Elements() {
			if stringValue(value) != "reasoning.encrypted_content" {
				return nil, refused
			}
		}
	}
	if compatibility {
		stored, _ := root.Lookup("store")
		parallel, _ := root.Lookup("parallel_tool_calls")
		if (stored.Kind() != oif.Absent && string(stored.Raw()) != "false") || (parallel.Kind() != oif.Absent && string(parallel.Raw()) != "true") {
			return nil, refused
		}
		cache, _ := root.Lookup("prompt_cache_key")
		if cache.Kind() != oif.Absent && (cache.Kind() != oif.String || len(cache.Raw()) > 16384) {
			return nil, refused
		}
		metadata, _ := root.Lookup("client_metadata")
		if metadata.Kind() != oif.Absent {
			if metadata.Kind() != oif.Object || len(metadata.Members()) > 64 {
				return nil, refused
			}
			for _, field := range metadata.Members() {
				if field.Value.Kind() != oif.String || len(field.Value.Raw()) > 16384 {
					return nil, refused
				}
			}
		}
		reasoning, _ := root.Lookup("reasoning")
		if reasoning.Kind() != oif.Absent {
			if reasoning.Kind() != oif.Object {
				return nil, refused
			}
			for _, field := range reasoning.Members() {
				if !slices.Contains([]string{"effort", "summary"}, field.Name) || stringValue(field.Value) != "none" {
					return nil, refused
				}
			}
		}
	}
	tools, _ := root.Lookup("tools")
	for _, tool := range tools.Elements() {
		kind, _ := tool.Lookup("type")
		strict, _ := tool.Lookup("strict")
		if stringValue(kind) != "function" || (strict.Kind() != oif.Absent && string(strict.Raw()) != "false") {
			return nil, refused
		}
	}
	input, _ := root.Lookup("input")
	if input.Kind() != oif.String && input.Kind() != oif.Array {
		return nil, refused
	}
	for _, item := range input.Elements() {
		if _, present := item.Lookup("internal_chat_message_metadata_passthrough"); present && (!compatibility || !codewire.SupportedResponsesItemAnnotation(item)) {
			return nil, refused
		}
		kind, _ := item.Lookup("type")
		switch stringValue(kind) {
		case "", "message":
			role, _ := item.Lookup("role")
			if !slices.Contains([]string{"user", "assistant", "system", "developer"}, stringValue(role)) || !grokTextContent(item, "content") {
				return nil, refused
			}
		case "function_call":
			if !grokString(item, "call_id") || !grokString(item, "name") || !grokString(item, "arguments") {
				return nil, refused
			}
		case "function_call_output":
			if !grokString(item, "call_id") || !grokString(item, "output") {
				return nil, refused
			}
		default:
			return nil, refused
		}
	}
	if !compatibility {
		return request, nil
	}
	document, err := oif.Apply(request.OIF().Document(), slices.Collect(func(yield func(oif.Change) bool) {
		for index, item := range input.Elements() {
			if _, present := item.Lookup("internal_chat_message_metadata_passthrough"); present && !yield(oif.Change{Pointer: oif.Pointer(oif.Pointer("/input", strconv.Itoa(index)), "internal_chat_message_metadata_passthrough"), Remove: true, Origin: oif.TransformedMapping, Reason: "published Responses compatibility policy"}) {
				return
			}
		}
	}))
	if err != nil {
		return nil, refused
	}
	prepared, err := request.WithFields(document.FieldsWithout("store", "parallel_tool_calls", "prompt_cache_key", "client_metadata", "reasoning", "include"), oif.TransformedMapping)
	if err != nil {
		return nil, refused
	}
	return prepared, nil
}

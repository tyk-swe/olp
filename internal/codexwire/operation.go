// Package codexwire observes Codex wire messages without rewriting them.
package codexwire

import (
	"net/http"
	"strconv"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
)

const MaxBody = codewire.MaxBody

type Request = codemode.Request

func Decode(raw []byte, encoding string, limit int64) ([]byte, error) {
	return codewire.Decode(raw, encoding, limit)
}

func Classify(body []byte, headers http.Header, path string, websocket bool) (Request, error) {
	return ClassifyWith(body, path, websocket, func(root oif.Value) (codemode.Identity, error) { return Identity(headers, root) })
}

// ClassifyWith classifies a Responses body whose conversation identity comes
// from identity, which reads the parsed body and its own request headers.
func ClassifyWith(body []byte, path string, websocket bool, identity func(oif.Value) (codemode.Identity, error)) (Request, error) {
	var request Request
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil || document.Root().Kind() != oif.Object {
		return request, codemode.Refuse(400, "code_body_invalid")
	}
	root := document.Root()
	conversation := field(root, "conversation")
	if conversation.Kind() != oif.Absent && conversation.Kind() != oif.Null {
		return request, codemode.Refuse(409, "code_parent_unresolved")
	}
	compact := false
	for _, item := range field(root, "input").Elements() {
		kind, _ := field(item, "type").Text()
		compact = compact || kind == "compaction_trigger"
		if kind == "item_reference" {
			return request, codemode.Refuse(409, "code_parent_unresolved")
		}
	}
	model, ok := field(root, "model").Text()
	if !ok || codemode.ValidateModels([]string{model}) != nil {
		return request, codemode.Refuse(400, "code_model_invalid")
	}
	request.Operation.Name = "responses"
	if path == "responses/compact" && !websocket || path == "responses" && compact {
		request.Operation.Name = "compact"
	} else if path != "responses" {
		return request, codemode.Refuse(400, "code_operation_unsupported")
	}
	if websocket {
		typ, _ := field(root, "type").Text()
		if typ != "response.create" {
			return request, codemode.Refuse(400, "code_operation_unsupported")
		}
		generate := field(root, "generate")
		if generate.Kind() != oif.Absent && generate.Kind() != oif.Boolean {
			return request, codemode.Refuse(400, "code_body_invalid")
		}
		if generate.Raw() == "false" {
			request.Operation.Name = "prewarm"
		}
	}
	for _, name := range []string{"background", "store"} {
		value := field(root, name)
		if value.Kind() != oif.Absent && value.Kind() != oif.Boolean {
			return request, codemode.Refuse(400, "code_body_invalid")
		}
		if name == "background" && value.Raw() == "true" {
			return request, codemode.Refuse(422, "code_operation_unsupported")
		}
	}
	request.Operation.Model = model
	identified, err := identity(root)
	if err != nil {
		return request, err
	}
	request.Operation.Identity = identified
	previous := field(root, "previous_response_id")
	if previous.Kind() != oif.Absent && previous.Kind() != oif.Null {
		request.PreviousResponse, ok = previous.Text()
		if !ok || (codemode.Identity{Conversation: request.PreviousResponse}).Validate() != nil {
			return request, codemode.Refuse(400, "code_reference_invalid")
		}
	}
	// This is a rate estimate only; opaque and server-side context has no proven bound.
	request.Estimate = max(1, int64(len(body))/4)
	if cap, err := strconv.ParseInt(field(root, "max_output_tokens").Raw(), 10, 64); err == nil && cap > 0 && cap <= 1<<32 {
		request.Estimate += cap
	}
	return request, nil
}

func Identity(headers http.Header, root oif.Value) (codemode.Identity, error) {
	var result codemode.Identity
	metadata := field(root, "client_metadata")
	if metadata.Kind() != oif.Absent && metadata.Kind() != oif.Null && metadata.Kind() != oif.Object {
		return result, codemode.Refuse(400, "code_identity_invalid")
	}
	merge := func(target *string, value string) bool {
		if value == "" {
			return true
		}
		if *target != "" && *target != value {
			return false
		}
		*target = value
		return true
	}
	readHeader := func(name string, target *string) bool {
		values := headers.Values(name)
		return len(values) == 0 || len(values) == 1 && values[0] != "" && merge(target, values[0])
	}
	readField := func(name string, target *string) bool {
		v := field(metadata, name)
		if v.Kind() == oif.Absent {
			return true
		}
		s, ok := v.Text()
		return ok && s != "" && merge(target, s)
	}
	if !readHeader("thread-id", &result.Conversation) || !readField("thread_id", &result.Conversation) || !readHeader("x-codex-parent-thread-id", &result.Parent) || !readField("x-codex-parent-thread-id", &result.Parent) {
		return result, codemode.Refuse(400, "code_identity_ambiguous")
	}
	if result.Conversation == "" && (!readHeader("session-id", &result.Conversation) || !readHeader("session_id", &result.Conversation)) {
		return result, codemode.Refuse(400, "code_identity_ambiguous")
	}
	var subagent string
	if !readHeader("x-openai-subagent", &subagent) || !readField("x-openai-subagent", &subagent) {
		return result, codemode.Refuse(400, "code_identity_ambiguous")
	}
	if subagent != "" && result.Parent == "" {
		return result, codemode.Refuse(409, "code_parent_unresolved")
	}
	return result, result.Validate()
}

func ConnectionIdentity(headers http.Header) (codemode.Identity, error) {
	return Identity(headers, oif.Value{})
}

func field(value oif.Value, name string) oif.Value { child, _ := value.Lookup(name); return child }

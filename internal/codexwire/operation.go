// Package codexwire observes Codex wire messages without rewriting them.
package codexwire

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

const MaxBody = 16 << 20

type Request struct {
	Operation        codemode.Operation
	PreviousResponse string
	Estimate         int64
}

func Decode(raw []byte, encoding string, limit int64) ([]byte, error) {
	if int64(len(raw)) > limit {
		return nil, codemode.Refuse(413, "code_body_too_large")
	}
	var reader io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, codemode.Refuse(400, "code_encoding_invalid")
		}
		defer gz.Close()
		reader = gz
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(limit)))
		if err != nil {
			return nil, codemode.Refuse(400, "code_encoding_invalid")
		}
		defer zr.Close()
		reader = zr
	default:
		return nil, codemode.Refuse(415, "code_encoding_unsupported")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, codemode.Refuse(400, "code_encoding_invalid")
	}
	if int64(len(body)) > limit {
		return nil, codemode.Refuse(413, "code_body_too_large")
	}
	return body, nil
}

func Classify(body []byte, headers http.Header, path string, websocket bool) (Request, error) {
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
	identity, err := Identity(headers, root)
	if err != nil {
		return request, err
	}
	request.Operation.Identity = identity
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

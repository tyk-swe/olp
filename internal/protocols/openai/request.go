// Package openai implements the OpenAI Chat Completions and Responses codecs:
// gateway envelope validation, upstream encoding with model rewriting, and
// unary/streaming response decoding with usage extraction.
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OperationGeneration is the gateway operation both codecs map to.
const OperationGeneration = "generation"

// Family selects the wire format of a generation request.
type Family string

const (
	FamilyChat           Family = "chat"
	FamilyResponses      Family = "responses"
	FamilyInputTokens    Family = "input_tokens"
	FamilyEmbeddings     Family = "embeddings"
	FamilyModeration     Family = "moderation"
	FamilyAnthropic      Family = "anthropic"
	FamilyAnthropicCount Family = "anthropic_count"
	FamilyGemini         Family = "gemini"
	FamilyGeminiStream   Family = "gemini_stream"
	FamilyGeminiCount    Family = "gemini_count"
	// Media families name their operation tag directly so the accounting
	// envelope and selection registry agree on the operation string.
	FamilyImageGeneration Family = "image_generation"
	FamilyImageEdit       Family = "image_edit"
	FamilyImageVariation  Family = "image_variation"
	FamilySpeech          Family = "speech"
	FamilyTranscription   Family = "transcription"
	FamilyVideoCreate     Family = "video_create"
	FamilyVideoList       Family = "video_list"
	FamilyVideoGet        Family = "video_get"
	FamilyVideoContent    Family = "video_content"
	FamilyVideoDelete     Family = "video_delete"
)

func (f Family) Operation() string {
	switch f {
	case FamilyInputTokens, FamilyAnthropicCount, FamilyGeminiCount:
		return "token_count"
	case FamilyEmbeddings:
		return "embeddings"
	case FamilyModeration:
		return "moderation"
	case FamilyImageGeneration, FamilyImageEdit, FamilyImageVariation, FamilySpeech,
		FamilyTranscription, FamilyVideoCreate, FamilyVideoList, FamilyVideoGet,
		FamilyVideoContent, FamilyVideoDelete:
		return string(f)
	}
	return OperationGeneration
}

func (f Family) Surface() string {
	switch f {
	case FamilyAnthropic, FamilyAnthropicCount:
		return "anthropic"
	case FamilyGemini, FamilyGeminiStream, FamilyGeminiCount:
		return "gemini"
	}
	return "openai"
}

// RouteSlug is the published-route identifier carried in the model field.
var RouteSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// RequestError rejects a request before any upstream work. It maps to an
// OpenAI invalid_request_error with HTTP 400.
type RequestError struct {
	Code    string
	Message string
	Param   string
}

func (e *RequestError) Error() string { return e.Message }

func invalid(param, message string) error {
	return &RequestError{Code: "invalid_value", Message: message, Param: param}
}

// Request is a validated generation request. The gateway decodes the fields
// it must understand and retains the whole document, including vendor
// extensions, for upstream encoding.
type Request struct {
	Family       Family
	Route        string
	Stream       bool
	IncludeUsage bool // the caller's chat-stream option, independent of upstream accounting
	fields       map[string]json.RawMessage
}

// Field returns a top-level field verbatim, or nil when absent.
func (r *Request) Field(name string) json.RawMessage { return r.fields[name] }

// Document returns a copy of the source envelope for a codec to rewrite.
func (r *Request) Document() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(r.fields))
	for k, v := range r.fields {
		out[k] = v
	}
	return out
}

// NewEnvelope is used by native codecs after validating their own wire grammar.
func NewEnvelope(family Family, route string, stream bool, fields map[string]json.RawMessage) *Request {
	return &Request{Family: family, Route: route, Stream: stream, fields: fields}
}

// Parse validates the gateway envelope of one request document.
func Parse(family Family, data []byte) (*Request, error) {
	fields, err := object(data)
	if err != nil {
		return nil, &RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	return parseFields(family, fields)
}

func parseFields(family Family, fields map[string]json.RawMessage) (*Request, error) {
	r := &Request{Family: family, fields: fields}
	model, ok := stringField(fields, "model")
	if !ok {
		return nil, &RequestError{Code: "missing_required_parameter", Message: "model must name a published route.", Param: "model"}
	}
	if !RouteSlug.MatchString(model) {
		return nil, invalid("model", "model must name a published route slug; provider-qualified names are not accepted.")
	}
	r.Route = model
	var err error
	if raw, present := fields["stream"]; present && !isNull(raw) {
		if err := json.Unmarshal(raw, &r.Stream); err != nil {
			return nil, invalid("stream", "stream must be a boolean.")
		}
	}
	switch family {
	case FamilyChat:
		err = r.validateChat()
	case FamilyResponses:
		err = validateResponses(fields)
	case FamilyInputTokens:
		err = validateResponses(fields)
	case FamilyEmbeddings, FamilyModeration:
		if raw, ok := fields["input"]; !ok || isNull(raw) {
			err = invalid("input", "input is required.")
		} else if _, ok := stringField(fields, "input"); !ok {
			if items, ok := arrayField(fields, "input"); !ok || len(items) == 0 {
				err = invalid("input", "input must be text or a non-empty array.")
			}
		}
		if family == FamilyEmbeddings {
			if !validEmbeddingInput(fields["input"]) {
				err = invalid("input", "Embedding input must be text, text arrays, token IDs, or batches of token IDs.")
			}
			if e := integerField(fields, "dimensions", true); e != nil {
				err = e
			}
			if raw, present := fields["encoding_format"]; present && !isNull(raw) {
				format, ok := stringField(fields, "encoding_format")
				if !ok || (format != "float" && format != "base64") {
					err = invalid("encoding_format", "encoding_format must be float or base64.")
				}
			}
		}
	default:
		return nil, errors.New("unknown request family")
	}
	if err != nil {
		return nil, err
	}
	if family.Operation() != OperationGeneration && r.Stream {
		return nil, invalid("stream", "This operation supports unary requests only.")
	}
	if raw, present := fields["top_logprobs"]; present && !isNull(raw) {
		n, ok := int64Field(fields, "top_logprobs")
		if !ok || n > 20 {
			return nil, invalid("top_logprobs", "top_logprobs must be an integer from 0 to 20.")
		}
	}
	return r, nil
}

func (r *Request) validateChat() error {
	fields := r.fields
	messages, ok := arrayField(fields, "messages")
	if !ok || len(messages) == 0 {
		return &RequestError{Code: "missing_required_parameter", Message: "messages must be a non-empty array.", Param: "messages"}
	}
	for i, raw := range messages {
		message, err := object(raw)
		if err != nil {
			return invalid(fmt.Sprintf("messages[%d]", i), "Each message must be an object.")
		}
		if role, ok := stringField(message, "role"); !ok || role == "" {
			return invalid(fmt.Sprintf("messages[%d].role", i), "Each message needs a role.")
		}
	}
	_, hasMax := fields["max_tokens"]
	_, hasMaxCompletion := fields["max_completion_tokens"]
	if hasMax && hasMaxCompletion {
		return invalid("max_tokens", "Send only one of max_tokens and max_completion_tokens.")
	}
	for _, name := range []string{"max_tokens", "max_completion_tokens", "n", "seed"} {
		if err := integerField(fields, name, name != "seed"); err != nil {
			return err
		}
	}
	for _, name := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
		if err := numberField(fields, name); err != nil {
			return err
		}
	}
	if raw, present := fields["stream_options"]; present && !isNull(raw) {
		options, err := object(raw)
		if err != nil {
			return invalid("stream_options", "stream_options must be an object.")
		}
		if raw, present := options["include_usage"]; present && !isNull(raw) {
			if err := json.Unmarshal(raw, &r.IncludeUsage); err != nil {
				return invalid("stream_options.include_usage", "include_usage must be a boolean.")
			}
		}
	}
	if err := toolsField(fields); err != nil {
		return err
	}
	if raw, present := fields["response_format"]; present && !isNull(raw) {
		if _, err := object(raw); err != nil {
			return invalid("response_format", "response_format must be an object.")
		}
	}
	return nil
}

func validateResponses(fields map[string]json.RawMessage) error {
	raw, present := fields["input"]
	if !present || isNull(raw) {
		return &RequestError{Code: "missing_required_parameter", Message: "input is required.", Param: "input"}
	}
	if _, ok := stringField(fields, "input"); !ok {
		items, ok := arrayField(fields, "input")
		if !ok || len(items) == 0 {
			return invalid("input", "input must be a string or a non-empty array.")
		}
		for i, raw := range items {
			if err := validateResponseInput(raw, fmt.Sprintf("input[%d]", i)); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"previous_response_id", "conversation"} {
		if raw, present := fields[name]; present && !isNull(raw) {
			return &RequestError{Code: "unsupported_stateful_reference", Message: "The gateway does not hold prior responses; " + name + " is not supported.", Param: name}
		}
	}
	if raw, present := fields["background"]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
		return &RequestError{Code: "unsupported_parameter", Message: "Background responses require stateful polling, which the gateway does not provide.", Param: "background"}
	}
	if raw, present := fields["instructions"]; present && !isNull(raw) {
		if _, ok := stringField(fields, "instructions"); !ok {
			return invalid("instructions", "instructions must be a string.")
		}
	}
	for _, name := range []string{"max_output_tokens", "max_tool_calls"} {
		if err := integerField(fields, name, true); err != nil {
			return err
		}
	}
	for _, name := range []string{"temperature", "top_p"} {
		if err := numberField(fields, name); err != nil {
			return err
		}
	}
	return toolsField(fields)
}

// validateResponseInput rejects account-scoped references while retaining
// inline messages and tool results for stateless replay across credentials.
func validateResponseInput(raw json.RawMessage, path string) error {
	item, err := object(raw)
	if err != nil {
		return invalid(path, "Each input item must be an object.")
	}
	kind, _ := stringField(item, "type")
	_, hasID := item["id"]
	// ItemReference permits type to be omitted or null. Inline messages can
	// omit type when they have no ID; replayed items with IDs need a type
	// so they cannot be interpreted as references by the provider.
	if kind == "item_reference" || (kind == "" && hasID) {
		return &RequestError{Code: "unsupported_stateful_reference", Message: "Input item references are not supported; send the full item inline.", Param: path}
	}
	if err := validateResponseFileReference(item, path); err != nil {
		return err
	}
	var field string
	switch kind {
	case "", "message":
		field = "content"
	case "function_call_output", "custom_tool_call_output", "computer_call_output":
		field = "output"
	default:
		return nil
	}
	parts, array := arrayField(item, field)
	if !array {
		// Computer screenshots are a single object; text inputs and tool
		// results remain opaque strings and are never interpreted as JSON.
		parts = []json.RawMessage{item[field]}
	}
	for i, raw := range parts {
		part, err := object(raw)
		if err != nil {
			continue
		}
		param := path + "." + field
		if array {
			param += fmt.Sprintf("[%d]", i)
		}
		if err := validateResponseFileReference(part, param); err != nil {
			return err
		}
	}
	return nil
}

func validateResponseFileReference(part map[string]json.RawMessage, path string) error {
	kind, _ := stringField(part, "type")
	switch kind {
	case "input_file", "input_image", "computer_screenshot":
		if raw, present := part["file_id"]; present && !isNull(raw) {
			return &RequestError{Code: "unsupported_stateful_reference", Message: "Provider file IDs are not supported; send file data or a URL instead.", Param: path + ".file_id"}
		}
	}
	return nil
}

func toolsField(fields map[string]json.RawMessage) error {
	raw, present := fields["tools"]
	if !present || isNull(raw) {
		return nil
	}
	tools, ok := arrayField(fields, "tools")
	if !ok {
		return invalid("tools", "tools must be an array.")
	}
	for i, tool := range tools {
		if _, err := object(tool); err != nil {
			return invalid(fmt.Sprintf("tools[%d]", i), "Each tool must be an object.")
		}
	}
	return nil
}

func integerField(fields map[string]json.RawMessage, name string, positive bool) error {
	raw, present := fields[name]
	if !present || isNull(raw) {
		return nil
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || (positive && n < 1) {
		return invalid(name, name+" must be a positive integer.")
	}
	return nil
}

func numberField(fields map[string]json.RawMessage, name string) error {
	raw, present := fields[name]
	if !present || isNull(raw) {
		return nil
	}
	if _, err := strconv.ParseFloat(string(bytes.TrimSpace(raw)), 64); err != nil {
		return invalid(name, name+" must be a number.")
	}
	return nil
}

// ValidateDefaults validates connection-wide defaults against both supported
// request families without treating upstream model identifiers as route slugs.
func ValidateDefaults(defaults map[string]json.RawMessage) error {
	fields := map[string]json.RawMessage{
		"model":    json.RawMessage(`"defaults-check"`),
		"messages": json.RawMessage(`[{"role":"user","content":"check"}]`),
		"input":    json.RawMessage(`"check"`),
	}
	for name, value := range defaults {
		if strings.HasPrefix(name, "/") || len(value) > 8192 {
			return invalid(name, "Invalid default name or oversized value.")
		}
		switch name {
		case "", "model", "stream", "messages", "input", "contents", "system", "systemInstruction", "generateContentRequest", "routing", "provider", "route", "headers", "tools", "functions":
			return invalid(name, "Defaults cannot set transport, routing, or input envelope fields.")
		}
		fields[name] = value
	}
	for _, family := range []Family{FamilyChat, FamilyResponses} {
		if _, err := parseFields(family, fields); err != nil {
			return err
		}
	}
	return nil
}

// Encode produces the upstream document: the retained fields, provider
// parameter defaults for absent keys, the upstream model, and usage reporting
// for chat streams so accounting never depends on client options.
func (r *Request) Encode(upstreamModel string, defaults map[string]json.RawMessage) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(r.fields)+len(defaults))
	for name, value := range defaults {
		out[name] = value
	}
	for name, value := range r.fields {
		out[name] = value
	}
	if r.Family == FamilyChat {
		// Either explicit token limit overrides the same setting under its alias,
		// including an explicit null that opts out of the provider default.
		if _, present := r.fields["max_tokens"]; present {
			delete(out, "max_completion_tokens")
		}
		if _, present := r.fields["max_completion_tokens"]; present {
			delete(out, "max_tokens")
		}
	}
	// Defaults are request fields too. Validate the merged envelope before
	// rewriting the route to an upstream identifier (which need not be a slug).
	if _, err := parseFields(r.Family, out); err != nil {
		return nil, err
	}
	model, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	out["model"] = model
	if r.Stream && r.Family == FamilyChat {
		options := map[string]json.RawMessage{}
		if raw, present := out["stream_options"]; present && !isNull(raw) {
			if options, err = object(raw); err != nil {
				return nil, err
			}
		}
		options["include_usage"] = json.RawMessage("true")
		if out["stream_options"], err = json.Marshal(options); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

// Extensions lists JSON pointers of fields the gateway does not model. They
// are forwarded verbatim; the list exists for diagnostics and tests.
func (r *Request) Extensions() []string {
	var paths []string
	known := chatKnown
	if r.Family == FamilyResponses {
		known = responsesKnown
	}
	walk(r.fields, "", known, &paths)
	sort.Strings(paths)
	return paths
}

type shape map[string]shape

var functionShape = shape{"name": nil, "description": nil, "parameters": nil}
var chatKnown = shape{
	"model": nil, "messages": shape{"*": shape{"role": nil, "content": shape{"*": shape{"type": nil, "text": nil, "image_url": nil, "input_audio": nil, "file": nil, "refusal": nil}}, "name": nil, "tool_calls": shape{"*": shape{"id": nil, "type": nil, "function": shape{"name": nil, "arguments": nil}}}, "tool_call_id": nil, "refusal": nil, "audio": nil, "function_call": nil}},
	"stream": nil, "stream_options": nil, "max_tokens": nil, "max_completion_tokens": nil, "temperature": nil, "top_p": nil, "n": nil, "stop": nil,
	"presence_penalty": nil, "frequency_penalty": nil, "logit_bias": nil, "logprobs": nil, "top_logprobs": nil, "user": nil, "seed": nil,
	"tools": shape{"*": shape{"type": nil, "function": functionShape}}, "tool_choice": nil, "parallel_tool_calls": nil,
	"response_format": shape{"type": nil, "json_schema": shape{"name": nil, "description": nil, "schema": nil, "strict": nil}},
	"metadata":        nil, "store": nil, "reasoning_effort": nil, "modalities": nil, "audio": nil, "prediction": nil, "web_search_options": nil, "verbosity": nil, "prompt_cache_key": nil, "safety_identifier": nil,
}
var responsesKnown = shape{
	"model": nil, "input": nil, "instructions": nil, "stream": nil, "stream_options": nil, "max_output_tokens": nil, "max_tool_calls": nil, "temperature": nil, "top_p": nil, "top_logprobs": nil,
	"tools": nil, "tool_choice": nil, "parallel_tool_calls": nil, "text": nil, "reasoning": nil, "metadata": nil, "store": nil, "background": nil, "truncation": nil, "user": nil, "include": nil,
	"previous_response_id": nil, "conversation": nil, "prompt": nil, "prompt_cache_key": nil, "safety_identifier": nil, "service_tier": nil,
}

func walk(fields map[string]json.RawMessage, prefix string, known shape, paths *[]string) {
	for name, raw := range fields {
		child, ok := known[name]
		if !ok {
			*paths = append(*paths, prefix+"/"+name)
			continue
		}
		if child == nil {
			continue
		}
		if wildcard, isList := child["*"]; isList {
			var items []json.RawMessage
			if json.Unmarshal(raw, &items) == nil {
				for i, item := range items {
					if nested, err := object(item); err == nil {
						walk(nested, prefix+"/"+name+"/"+strconv.Itoa(i), wildcard, paths)
					}
				}
			}
			continue
		}
		if nested, err := object(raw); err == nil {
			walk(nested, prefix+"/"+name, child, paths)
		}
	}
}

func object(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, errors.New("expected a JSON object")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("expected one JSON document")
	}
	return fields, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func stringField(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, present := fields[name]
	if !present || isNull(raw) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func arrayField(fields map[string]json.RawMessage, name string) ([]json.RawMessage, bool) {
	raw, present := fields[name]
	if !present {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, false
	}
	return items, true
}

func int64Field(fields map[string]json.RawMessage, name string) (int64, bool) {
	raw, present := fields[name]
	if !present || isNull(raw) {
		return 0, false
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

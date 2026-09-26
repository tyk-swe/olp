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
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

// OperationGeneration is the gateway operation both codecs map to.
const OperationGeneration = "generation"

// Family selects the wire format of a generation request.
type Family string

const (
	FamilyChat               Family = "chat"
	FamilyResponses          Family = "responses"
	FamilyInputTokens        Family = "input_tokens"
	FamilyEmbeddings         Family = "embeddings"
	FamilyModeration         Family = "moderation"
	FamilyAnthropic          Family = "anthropic"
	FamilyAnthropicCount     Family = "anthropic_count"
	FamilyGemini             Family = "gemini"
	FamilyGeminiStream       Family = "gemini_stream"
	FamilyGeminiCount        Family = "gemini_count"
	FamilyGeminiInteractions Family = "gemini_interactions"
	FamilyGeminiLive         Family = "gemini_live"

	FamilyGeminiEmbeddings      Family = "gemini_embeddings"
	FamilyGeminiEmbeddingsBatch Family = "gemini_embeddings_batch"
	FamilyVertexEmbeddings      Family = "vertex_embeddings"
	FamilyBedrockEmbeddings     Family = "bedrock_embeddings"
	FamilyRerank                Family = "rerank"
	// Media families name their operation tag directly so the accounting
	// envelope and selection registry agree on the operation string.
	FamilyImageGeneration Family = "image_generation"
	FamilyImageEdit       Family = "image_edit"
	FamilyImageVariation  Family = "image_variation"
	FamilySpeech          Family = "speech"
	FamilyTranscription   Family = "transcription"
	FamilyTranslation     Family = "translation"
	FamilyVideoCreate     Family = "video_create"
	FamilyVideoList       Family = "video_list"
	FamilyVideoGet        Family = "video_get"
	FamilyVideoContent    Family = "video_content"
	FamilyVideoDelete     Family = "video_delete"

	FamilyFile          Family = "file"
	FamilyBatch         Family = "batch"
	FamilyRealtime      Family = "realtime"
	FamilyBedrock       Family = "bedrock"
	FamilyBedrockInvoke Family = "bedrock_invoke"
)

func (f Family) Operation() string {
	switch f {
	case FamilyInputTokens, FamilyAnthropicCount, FamilyGeminiCount:
		return "token_count"
	case FamilyEmbeddings, FamilyGeminiEmbeddings, FamilyGeminiEmbeddingsBatch,
		FamilyVertexEmbeddings, FamilyBedrockEmbeddings:
		return "embeddings"
	case FamilyModeration:
		return "moderation"
	case FamilyRerank:
		return "rerank"
	case FamilyImageGeneration, FamilyImageEdit, FamilyImageVariation, FamilySpeech,
		FamilyTranscription, FamilyTranslation, FamilyVideoCreate, FamilyVideoList, FamilyVideoGet,
		FamilyVideoContent, FamilyVideoDelete, FamilyFile, FamilyBatch,
		FamilyRealtime, FamilyGeminiLive, FamilyBedrockInvoke:
		if f == FamilyGeminiLive {
			return "realtime"
		}
		return string(f)
	}
	return OperationGeneration
}

func (f Family) Surface() string {
	switch f {
	case FamilyAnthropic, FamilyAnthropicCount:
		return "anthropic"
	case FamilyGemini, FamilyGeminiStream, FamilyGeminiCount, FamilyGeminiInteractions, FamilyGeminiLive:
		return "gemini"
	case FamilyBedrock, FamilyBedrockInvoke:
		return "bedrock"
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
	Family            Family
	Route             string
	Stream            bool
	IncludeUsage      bool // the caller's chat-stream option, independent of upstream accounting
	source            oif.Request
	sourceError       error
	validatedDocument oif.Document
	validatedFamily   Family
}

// Field returns a top-level field verbatim, or nil when absent.
func (r *Request) Field(name string) json.RawMessage {
	value, _ := r.source.Document().Root().Lookup(name)
	return value.Bytes()
}

func (r *Request) SetField(name string, value json.RawMessage) {
	next, err := r.source.WithChanges(oif.Change{Pointer: oif.Pointer("", name), Value: string(value), Origin: oif.ResourceBinding, Reason: "gateway resource reference"})
	if err != nil {
		r.sourceError = err
		return
	}
	r.source = next
}

func (r *Request) OIF() oif.Request { return r.source }

// Document returns a copy of the source envelope for a codec to rewrite.
func (r *Request) Document() map[string]json.RawMessage {
	return r.source.Document().Fields()
}

// NewEnvelope is used by native codecs after validating their own wire grammar.
func NewEnvelope(family Family, route string, stream bool, fields map[string]json.RawMessage) *Request {
	data, err := json.Marshal(fields)
	if err != nil {
		return &Request{Family: family, Route: route, Stream: stream, sourceError: err}
	}
	doc, err := oif.ParseJSON(data, oif.Limits{})
	if err != nil {
		return &Request{Family: family, Route: route, Stream: stream, sourceError: err}
	}
	return NewSourceEnvelope(family, route, stream, doc)
}

func NewSourceEnvelope(family Family, route string, stream bool, doc oif.Document) *Request {
	descriptor := Descriptor(family, stream)
	source, err := oif.NewRequest(descriptor, doc)
	return &Request{Family: family, Route: route, Stream: stream, source: source, sourceError: err}
}

// WithFields retains the immutable caller source when an admitted policy
// produces a different effective document. It records only changed fields.
func (r *Request) WithFields(fields map[string]json.RawMessage, origin oif.Origin) *Request {
	out := *r
	changes := []oif.Change{}
	for name, value := range fields {
		if !bytes.Equal(r.Field(name), value) {
			changes = append(changes, oif.Change{Pointer: oif.Pointer("", name), Value: string(value), Origin: origin, Reason: "explicit input policy"})
		}
	}
	for _, m := range r.source.Document().Root().Members() {
		if _, ok := fields[m.Name]; !ok {
			changes = append(changes, oif.Change{Pointer: oif.Pointer("", m.Name), Remove: true, Origin: origin, Reason: "explicit input policy"})
		}
	}
	if len(changes) > 0 {
		out.source, out.sourceError = r.source.WithChanges(changes...)
	}
	return &out
}

// Parse validates the gateway envelope of one request document.
func Parse(family Family, data []byte) (*Request, error) {
	doc, err := oif.ParseJSON(data, oif.Limits{})
	if err != nil {
		return nil, &RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	return parseDocument(family, doc)
}

func parseFields(family Family, fields map[string]json.RawMessage) (*Request, error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	doc, err := oif.ParseJSON(data, oif.Limits{})
	if err != nil {
		return nil, err
	}
	return parseDocument(family, doc)
}
func parseDocument(family Family, doc oif.Document) (*Request, error) {
	fields := doc.Fields()
	if fields == nil {
		return nil, &RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	r := NewSourceEnvelope(family, "", false, doc)
	if err := r.validateFields(fields); err != nil {
		return nil, err
	}
	r.source = r.source.WithDescriptor(Descriptor(family, r.Stream))
	r.validatedDocument, r.validatedFamily = doc, family
	return r, nil
}

func (r *Request) validateFields(fields map[string]json.RawMessage) error {
	family := r.Family
	r.Stream, r.IncludeUsage = false, false
	model, ok := stringField(fields, "model")
	if !ok {
		return &RequestError{Code: "missing_required_parameter", Message: "model must name a published route.", Param: "model"}
	}
	if !RouteSlug.MatchString(model) {
		return invalid("model", "model must name a published route slug; provider-qualified names are not accepted.")
	}
	r.Route = model
	var err error
	if raw, present := fields["stream"]; present && !isNull(raw) {
		if err := json.Unmarshal(raw, &r.Stream); err != nil {
			return invalid("stream", "stream must be a boolean.")
		}
	}
	switch family {
	case FamilyChat:
		err = r.validateChat(fields)
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
	case FamilyRerank:
		err = r.validateRerank(fields)
	default:
		return errors.New("unknown request family")
	}
	if err != nil {
		return err
	}
	if family.Operation() != OperationGeneration && r.Stream {
		return invalid("stream", "This operation supports unary requests only.")
	}
	if raw, present := fields["top_logprobs"]; present && !isNull(raw) {
		n, ok := int64Field(fields, "top_logprobs")
		if !ok || n > 20 {
			return invalid("top_logprobs", "top_logprobs must be an integer from 0 to 20.")
		}
	}
	return nil
}

func (r *Request) validateChat(fields map[string]json.RawMessage) error {
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
	if raw, present := fields["previous_response_id"]; present && !isNull(raw) {
		if _, ok := stringField(fields, "previous_response_id"); !ok {
			return invalid("previous_response_id", "previous_response_id must be a string.")
		}
	}
	if raw, present := fields["conversation"]; present && !isNull(raw) {
		return &RequestError{Code: "unsupported_stateful_reference", Message: "The gateway does not hold prior conversations; conversation is not supported.", Param: "conversation"}
	}
	for _, name := range []string{"background", "store"} {
		if raw, present := fields[name]; present && !isNull(raw) {
			var flag bool
			if json.Unmarshal(raw, &flag) != nil {
				return invalid(name, name+" must be a boolean.")
			}
		}
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
	return r.encode(upstreamModel, defaults, nil)
}
func (r *Request) EncodeWithProvenance(upstreamModel string, defaults map[string]json.RawMessage) ([]byte, []oif.Provenance, error) {
	fields, provenance, err := r.EncodeFieldsWithProvenance(upstreamModel, defaults)
	if err != nil {
		return nil, provenance, err
	}
	body, err := json.Marshal(fields)
	return body, provenance, err
}

// EncodeFieldsWithProvenance gives the transformed dialect codec its owned
// destination fields without a serialize/parse round trip. The source remains
// immutable; profile-specific transformed lowering can inspect this working copy.
func (r *Request) EncodeFieldsWithProvenance(upstreamModel string, defaults map[string]json.RawMessage) (map[string]json.RawMessage, []oif.Provenance, error) {
	var provenance []oif.Provenance
	fields, err := r.encodeFields(upstreamModel, defaults, func(name string) {
		provenance = append(provenance, oif.Provenance{Pointer: oif.Pointer("", name), Origin: oif.ProviderDefault, Reason: "absent caller field inherited provider default"})
	})
	return fields, provenance, err
}
func (r *Request) encode(upstreamModel string, defaults map[string]json.RawMessage, applied func(string)) ([]byte, error) {
	fields, err := r.encodeFields(upstreamModel, defaults, applied)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}
func (r *Request) encodeFields(upstreamModel string, defaults map[string]json.RawMessage, applied func(string)) (map[string]json.RawMessage, error) {
	if r.sourceError != nil {
		return nil, r.sourceError
	}
	fields := r.Document()
	out := make(map[string]json.RawMessage, len(fields)+len(defaults))
	for name, value := range defaults {
		out[name] = bytes.Clone(value)
	}
	maps.Copy(out, fields)
	if r.Family == FamilyChat {
		// Either explicit token limit overrides the same setting under its alias,
		// including an explicit null that opts out of the provider default.
		if _, present := fields["max_tokens"]; present {
			delete(out, "max_completion_tokens")
		}
		if _, present := fields["max_completion_tokens"]; present {
			delete(out, "max_tokens")
		}
	}

	if r.Family == FamilyResponses || r.Family == FamilyInputTokens {
		for _, name := range []string{"previous_response_id", "conversation", "background", "store"} {
			if _, injected := defaults[name]; injected {
				if _, caller := fields[name]; !caller {
					return nil, &RequestError{Code: "unsupported_stateful_reference", Message: "Provider defaults cannot supply " + name + ".", Param: name}
				}
			}
		}
	}
	if applied != nil {
		for name := range defaults {
			_, caller := fields[name]
			_, retained := out[name]
			if !caller && retained {
				applied(name)
			}
		}
	}
	// Defaults are request fields too. Validate the merged envelope before
	// rewriting the route to an upstream identifier (which need not be a slug).
	// Only the exact immutable document and dialect validated by Parse can
	// reuse that result. Constructors, overlays and defaults still validate.
	if len(defaults) > 0 || r.validatedFamily != r.Family || r.validatedDocument != r.source.Document() {
		validation := *r
		if err := validation.validateFields(out); err != nil {
			return nil, err
		}
	}
	model, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	out["model"] = model
	// Keep the established Responses wire normalization while preserving
	// array inputs and native extension fields without translation.
	if r.Family == FamilyResponses || r.Family == FamilyInputTokens {
		if text, ok := stringField(out, "input"); ok {
			out["input"], _ = json.Marshal([]map[string]any{{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": text}},
			}})
		}
	}
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
	return out, nil
}

// Extensions lists JSON pointers of fields the gateway does not model. They
// are forwarded verbatim; the list exists for diagnostics and tests.
func (r *Request) Extensions() []string {
	var paths []string
	known := chatKnown
	if r.Family == FamilyResponses {
		known = responsesKnown
	}
	walk(r.Document(), "", known, &paths)
	slices.Sort(paths)
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

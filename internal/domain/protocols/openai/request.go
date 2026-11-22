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
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
)

func invalid(param, message string) error {
	return &wire.RequestError{Code: "invalid_value", Message: message, Param: param}
}

// Parse validates the gateway envelope of one request document.
func Parse(family wire.Format, data []byte) (*wire.Request, error) {
	return ParseForModel(family, data, "")
}

func ParseForModel(family wire.Format, data []byte, model string) (*wire.Request, error) {
	doc, err := ParseSource(data)
	if err != nil {
		return nil, err
	}
	return ParseDocumentForModel(family, doc, model)
}

func ParseSource(data []byte) (oif.Document, error) {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return oif.Document{}, &wire.RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	doc, err := oif.ParseJSON(data, oif.Limits{})
	if err != nil {
		return oif.Document{}, &wire.RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	return doc, nil
}

func ParseDocumentForModel(family wire.Format, doc oif.Document, model string) (*wire.Request, error) {
	return parseDocument(family, doc, model)
}

func parseFields(family wire.Format, fields map[string]json.RawMessage) (*wire.Request, error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	doc, err := oif.ParseJSON(data, oif.Limits{})
	if err != nil {
		return nil, err
	}
	return parseDocument(family, doc, "")
}
func parseDocument(family wire.Format, doc oif.Document, expectedModel string) (*wire.Request, error) {
	// The members that carry the conversation are read from the document, so
	// they are not copied into the fields.
	var read []string
	switch family {
	case wire.Chat:
		read = []string{"messages"}
	case wire.Responses, wire.InputTokens:
		read = []string{"input"}
	}
	fields := doc.FieldsWithout(read...)
	if fields == nil {
		return nil, &wire.RequestError{Code: "invalid_json", Message: "The request body must be one JSON object."}
	}
	r, err := wire.NewRequest(family, "", false, doc)
	if err != nil {
		return nil, err
	}
	if err := validateFields(r, fields, doc.Root(), expectedModel); err != nil {
		return nil, err
	}

	r = r.Validated()
	return r, nil
}

// validateFields checks the envelope of a request. The document, when there is
// one, is the parsed form of fields, which the members that carry a conversation
// are read from in place of the copies in fields: they are the same text, and a
// prompt of 400 KB is not scanned again for each level of the conversation it is
// nested in. It is the zero value for fields that are not those of a document.
func validateFields(r *wire.Request, fields map[string]json.RawMessage, doc oif.Value, expectedModel string) error {
	family := r.Family
	r.Stream, r.IncludeUsage = false, false
	model, ok := stringField(fields, "model")
	if !ok {
		return &wire.RequestError{Code: "missing_required_parameter", Message: "model must name a published route.", Param: "model"}
	}
	if (expectedModel == "" && !routeidentity.ValidSlug(model)) || (expectedModel != "" && model != expectedModel) {
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
	case wire.Completions:
		err = validateCompletions(r, fields)
	case wire.Chat:
		err = validateChat(r, fields, doc)
	case wire.Responses:
		err = validateResponses(fields, doc)
	case wire.InputTokens:
		err = validateResponses(fields, doc)
	case wire.Embeddings, wire.Moderation:
		if raw, ok := fields["input"]; !ok || isNull(raw) {
			err = invalid("input", "input is required.")
		} else if _, ok := stringField(fields, "input"); !ok {
			if items, ok := arrayField(fields, "input"); !ok || len(items) == 0 {
				err = invalid("input", "input must be text or a non-empty array.")
			}
		}
		if family == wire.Embeddings {
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
	case wire.Rerank:
		err = validateRerank(fields)
	default:
		return errors.New("unknown request family")
	}
	if err != nil {
		return err
	}
	if family.Operation() != wire.OperationGeneration && r.Stream {
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

func validateChat(r *wire.Request, fields map[string]json.RawMessage, doc oif.Value) error {
	if err := validateChatMessages(fields, doc); err != nil {
		return err
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

// validateChatMessages checks that messages is a non-empty array of objects that
// each have a role.
func validateChatMessages(fields map[string]json.RawMessage, doc oif.Value) error {
	if doc.Kind() == oif.Object {
		messages, _ := doc.Lookup("messages")
		elements := messages.Elements()
		if len(elements) == 0 {
			return &wire.RequestError{Code: "missing_required_parameter", Message: "messages must be a non-empty array.", Param: "messages"}
		}
		for i, message := range elements {
			if message.Kind() != oif.Object {
				return invalid(fmt.Sprintf("messages[%d]", i), "Each message must be an object.")
			}
			role, _ := message.Lookup("role")
			if text, ok := role.Chars(); !ok || text == "" {
				return invalid(fmt.Sprintf("messages[%d].role", i), "Each message needs a role.")
			}
		}
		return nil
	}
	messages, ok := arrayField(fields, "messages")
	if !ok || len(messages) == 0 {
		return &wire.RequestError{Code: "missing_required_parameter", Message: "messages must be a non-empty array.", Param: "messages"}
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
	for _, family := range []wire.Format{wire.Chat, wire.Responses} {
		if _, err := parseFields(family, fields); err != nil {
			return err
		}
	}
	return nil
}

// EncodeFieldsWithProvenance gives the transformed dialect codec its owned
// destination fields without a serialize/parse round trip. The source remains
// immutable; profile-specific transformed lowering can inspect this working copy.
func EncodeFieldsWithProvenance(r *wire.Request, upstreamModel string, defaults map[string]json.RawMessage) (map[string]json.RawMessage, []oif.Provenance, error) {
	var provenance []oif.Provenance
	fields, err := encodeFields(r, upstreamModel, defaults, func(name string) {
		provenance = append(provenance, oif.Provenance{Pointer: oif.Pointer("", name), Origin: oif.ProviderDefault, Reason: "absent caller field inherited provider default"})
	})
	return fields, provenance, err
}
func encodeFields(r *wire.Request, upstreamModel string, defaults map[string]json.RawMessage, applied func(string)) (map[string]json.RawMessage, error) {
	fields := r.Document()
	out := make(map[string]json.RawMessage, len(fields)+len(defaults))
	for name, value := range defaults {
		out[name] = bytes.Clone(value)
	}
	maps.Copy(out, fields)
	if r.Family == wire.Chat {
		// Either explicit token limit overrides the same setting under its alias,
		// including an explicit null that opts out of the provider default.
		if _, present := fields["max_tokens"]; present {
			delete(out, "max_completion_tokens")
		}
		if _, present := fields["max_completion_tokens"]; present {
			delete(out, "max_tokens")
		}
	}

	if r.Family == wire.Responses || r.Family == wire.InputTokens {
		for _, name := range []string{"previous_response_id", "conversation", "background", "store"} {
			if _, injected := defaults[name]; injected {
				if _, caller := fields[name]; !caller {
					return nil, &wire.RequestError{Code: "unsupported_stateful_reference", Message: "Provider defaults cannot supply " + name + ".", Param: name}
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
	if len(defaults) > 0 || !r.IsValidated() {
		validation := *r
		if err := validateFields(&validation, out, oif.Value{}, ""); err != nil {
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
	if r.Family == wire.Responses || r.Family == wire.InputTokens {
		if text, ok := stringField(out, "input"); ok {
			out["input"], _ = json.Marshal([]map[string]any{{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": text}},
			}})
		}
	}
	if r.Stream && (r.Family == wire.Chat || r.Family == wire.Completions) {
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

func object(data []byte) (map[string]json.RawMessage, error) {
	// What nearly every call is given is one object, which is read in one go. The
	// decoder, which copies what it reads into a buffer of its own, only says
	// what is wrong with anything else.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil && fields != nil {
		return fields, nil
	}
	return explainedObject(data)
}

// explainedObject is object for input that is not one object, and says why it is
// not.
func explainedObject(data []byte) (map[string]json.RawMessage, error) {
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

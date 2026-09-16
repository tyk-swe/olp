// Package protocols connects native wire codecs through the shared operation model.
// Source documents are retained for native calls; translation must account for
// every semantic request field before an upstream request can be dispatched.
package protocols

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type Object = map[string]json.RawMessage

func object(data []byte) (Object, error) {
	var v Object
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&v); err != nil || v == nil {
		return nil, requestError("", "expected a JSON object")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, requestError("", "expected one JSON document")
	}
	return v, nil
}
func raw(v any) json.RawMessage    { b, _ := json.Marshal(v); return b }
func str(v json.RawMessage) string { var s string; _ = json.Unmarshal(v, &s); return s }
func arr(v json.RawMessage) []json.RawMessage {
	var a []json.RawMessage
	_ = json.Unmarshal(v, &a)
	return a
}
func present(v json.RawMessage) bool {
	return len(v) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null"))
}
func requestError(field, message string) error {
	return &openai.RequestError{Code: "unsupported_parameter", Param: field, Message: message}
}
func protocolError(message string) error { return &openai.ProtocolError{Detail: message} }

// Parse validates a client envelope. Gemini's model and transport are path
// parameters, so body fields can never replace them.
func Parse(family openai.Family, data []byte, model string) (*openai.Request, error) {
	if family.Surface() == "openai" {
		return openai.Parse(family, data)
	}
	f, err := object(data)
	if err != nil {
		return nil, err
	}
	stream := family == openai.FamilyGeminiStream
	if family.Surface() == "anthropic" {
		model = str(f["model"])
		if present(f["stream"]) {
			if err := json.Unmarshal(f["stream"], &stream); err != nil {
				return nil, requestError("stream", "stream must be a boolean")
			}
		}
		if len(arr(f["messages"])) == 0 {
			return nil, requestError("messages", "messages must be a non-empty array")
		}
		if family.Operation() == "generation" {
			var n int64
			if json.Unmarshal(f["max_tokens"], &n) != nil || n < 1 {
				return nil, requestError("max_tokens", "max_tokens must be a positive integer")
			}
		}
		if err := validateNativeControls(f, false); err != nil {
			return nil, err
		}
		for _, m := range arr(f["messages"]) {
			msg, e := object(m)
			if e != nil {
				return nil, e
			}
			if role := str(msg["role"]); role != "user" && role != "assistant" {
				return nil, requestError("messages.role", "Anthropic messages require user or assistant roles")
			}
			if !present(msg["content"]) {
				return nil, requestError("messages.content", "message content is required")
			}
		}
	} else {
		if _, ok := f["model"]; ok {
			return nil, requestError("model", "Gemini model belongs in the request path")
		}
		if _, ok := f["stream"]; ok {
			return nil, requestError("stream", "Gemini streaming is selected by the request path")
		}
		content := f
		if present(f["generateContentRequest"]) {
			if family != openai.FamilyGeminiCount || present(f["contents"]) {
				return nil, requestError("generateContentRequest", "countTokens accepts either contents or generateContentRequest")
			}
			content, err = object(f["generateContentRequest"])
			if err != nil {
				return nil, err
			}
		}
		if err := validateNativeControls(content, true); err != nil {
			return nil, err
		}
		if len(arr(content["contents"])) == 0 {
			return nil, requestError("contents", "contents must be a non-empty array")
		}
		for _, m := range arr(content["contents"]) {
			msg, e := object(m)
			if e != nil {
				return nil, e
			}
			if len(arr(msg["parts"])) == 0 {
				return nil, requestError("contents.parts", "content parts must be a non-empty array")
			}
		}
	}
	if !openai.RouteSlug.MatchString(model) {
		return nil, requestError("model", "model must name a published route slug")
	}
	if family.Operation() != "generation" && stream {
		return nil, requestError("stream", "token counting supports unary requests only")
	}
	return openai.NewEnvelope(family, model, stream, f), nil
}

// WireFamily selects the provider protocol while retaining the OpenAI endpoint
// hint where that connector promises native Responses support.
func WireFamily(kind, vendor string, source openai.Family) openai.Family {
	count := source.Operation() == "token_count"
	switch kind {
	case "anthropic":
		if count {
			return openai.FamilyAnthropicCount
		}
		return openai.FamilyAnthropic
	case "gemini", "vertex_ai":
		if count {
			return openai.FamilyGeminiCount
		}
		return openai.FamilyGemini
	case "bedrock":
		if count {
			return "bedrock_count"
		}
		return "bedrock"
	}
	if count {
		return openai.FamilyInputTokens
	}
	if source.Operation() == "embeddings" {
		return openai.FamilyEmbeddings
	}
	if source.Operation() == "moderation" {
		return openai.FamilyModeration
	}
	if source == openai.FamilyResponses && !ChatOnly(vendor) {
		return source
	}
	return openai.FamilyChat
}
func ChatOnly(vendor string) bool {
	switch vendor {
	case "deepseek", "fireworks", "deepinfra", "huggingface", "perplexity", "cohere":
		return true
	}
	return false
}

// Encode applies connector defaults before translation, and keeps native
// extensions byte-for-byte as JSON values. The caller always owns model and mode.
func Encode(r *openai.Request, kind, vendor, model string, defaults Object) ([]byte, openai.Family, error) {
	wire := WireFamily(kind, vendor, r.Family)
	f := r.Document()
	native := wire == r.Family || (wire == openai.FamilyGemini && r.Family == openai.FamilyGeminiStream)
	sourceDefaults := defaults
	if !native {
		sourceDefaults = nil
	}
	if vendor == "voyage" {
		if present(r.Field("dimensions")) && present(r.Field("output_dimension")) {
			return nil, wire, requestError("dimensions", "Use only one embedding dimension parameter")
		}
		copyDefaults := Object{}
		for k, v := range sourceDefaults {
			copyDefaults[k] = v
		}
		sourceDefaults = copyDefaults
		if len(r.Field("dimensions")) > 0 {
			delete(sourceDefaults, "output_dimension")
		}
		if len(r.Field("output_dimension")) > 0 {
			delete(sourceDefaults, "dimensions")
		}
	}
	mergeDefaults(f, sourceDefaults)
	if r.Family.Surface() == "openai" {
		encoded, err := r.Encode(model, sourceDefaults)
		if err != nil {
			return nil, wire, err
		}
		f, err = object(encoded)
		if err != nil {
			return nil, wire, err
		}
	}
	if err := validateProfile(vendor, r.Family, f); err != nil {
		return nil, wire, err
	}
	if native {
		if r.Family.Surface() != "gemini" {
			f["model"] = raw(model)
		} else if present(f["generateContentRequest"]) {
			nested, _ := object(f["generateContentRequest"])
			nested["model"] = raw("models/" + strings.TrimPrefix(model, "models/"))
			f["generateContentRequest"] = raw(nested)
		}
		if vendor == "voyage" {
			normalizeVoyage(f)
		}
		if ChatOnly(vendor) {
			if value, ok := f["max_completion_tokens"]; ok {
				f["max_tokens"] = value
				delete(f, "max_completion_tokens")
			}
		}
		encoded, err := json.Marshal(f)
		return encoded, wire, err
	}
	c, err := decodeCanonical(r.Family, f)
	if err != nil {
		return nil, wire, err
	}
	if len(c.Extensions) > 0 {
		return nil, wire, requestError(c.Extensions[0], "The selected provider cannot preserve request semantics at "+strings.Join(c.Extensions, ", "))
	}
	c.Stream = r.Stream
	if wire == openai.FamilyAnthropic && !present(c.Parameters["max_output_tokens"]) {
		if _, explicit := f["max_tokens"]; !explicit {
			c.Parameters["max_output_tokens"] = defaults["max_tokens"]
		}
	}
	out, err := encodeCanonical(c, wire, model, r.Family.Operation())
	if err != nil {
		return nil, wire, err
	}
	targetDefaults := Object{}
	for k, v := range defaults {
		targetDefaults[k] = v
	}
	if present(c.Parameters["max_output_tokens"]) {
		delete(targetDefaults, "max_tokens")
		delete(targetDefaults, "max_completion_tokens")
		delete(targetDefaults, "max_output_tokens")
	}
	for _, key := range []string{"temperature", "top_p", "seed", "stop", "max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, ok := f[key]; ok && !present(v) {
			delete(targetDefaults, key)
			if strings.HasPrefix(key, "max_") {
				delete(targetDefaults, "max_tokens")
				delete(targetDefaults, "max_completion_tokens")
				delete(targetDefaults, "max_output_tokens")
			}
		}
	}
	mergeDefaults(out, targetDefaults)
	if ChatOnly(vendor) {
		if value, ok := out["max_completion_tokens"]; ok {
			out["max_tokens"] = value
			delete(out, "max_completion_tokens")
		}
	}
	if err := validateProfile(vendor, wire, out); err != nil {
		return nil, wire, err
	}
	encoded, err := json.Marshal(out)
	return encoded, wire, err
}
func validateProfile(vendor string, family openai.Family, f Object) error {
	op := family.Operation()
	if vendor == "voyage" && op != "embeddings" || vendor == "cohere" && op != "generation" && op != "embeddings" || ChatOnly(vendor) && vendor != "cohere" && op != "generation" {
		return requestError("operation", "Operation is outside the configured vendor contract")
	}
	if vendor == "cohere" {
		for _, k := range []string{"n", "parallel_tool_calls", "dimensions", "user", "store", "metadata", "logit_bias", "top_logprobs", "modalities", "prediction", "audio", "service_tier", "input_type", "truncate"} {
			if present(f[k]) {
				return requestError(k, "Cohere cannot represent "+k)
			}
		}
	}
	if vendor == "voyage" {
		if _, ok := textInput(f["input"]); !ok {
			return requestError("input", "Voyage embeddings require text input")
		}
	}
	return nil
}
func textInput(v json.RawMessage) ([]string, bool) {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return []string{s}, true
	}
	var a []string
	err := json.Unmarshal(v, &a)
	return a, err == nil && len(a) > 0
}
func normalizeVoyage(f Object) {
	if v, ok := f["dimensions"]; ok {
		f["output_dimension"] = v
		delete(f, "dimensions")
	}
	if _, ok := f["encoding_format"]; ok {
		f["output_dtype"] = raw("float")
		delete(f, "encoding_format")
	}
}

// ParameterNames is used by strict routing policies. Delivery fields never
// become model requirements; nested native extensions remain affirmative facts.
func ParameterNames(r *openai.Request) []string {
	c, err := decodeCanonical(r.Family, r.Document())
	if err != nil {
		return []string{"unsupported_semantics"}
	}
	names := make([]string, 0, len(c.Parameters)+len(c.Extensions))
	for k := range c.Parameters {
		names = append(names, k)
	}
	if len(c.Tools) > 0 {
		names = append(names, "tools")
	}
	for _, p := range c.Extensions {
		names = append(names, strings.TrimPrefix(p, "/"))
	}
	return names
}

func unsupported(path string) error {
	return requestError(path, fmt.Sprintf("The selected protocol cannot represent %s", path))
}

func mergeDefaults(fields, defaults Object) {
	for key, value := range defaults {
		current, exists := fields[key]
		if !exists {
			fields[key] = value
			continue
		}
		if key == "generationConfig" && present(current) {
			caller, e := object(current)
			fallback, err := object(value)
			if e == nil && err == nil {
				for k, v := range fallback {
					if _, ok := caller[k]; !ok {
						caller[k] = v
					}
				}
				fields[key] = raw(caller)
			}
		}
	}
}

func EmbeddingEncoding(r *openai.Request, defaults Object) string {
	value := r.Field("encoding_format")
	if len(value) == 0 {
		value = defaults["encoding_format"]
	}
	return str(value)
}

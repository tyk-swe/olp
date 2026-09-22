package tokenization

import (
	"encoding/base64"
	"math/big"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func countRequest(source oif.Request, id string) (oif.View, error) {
	r := CountRequest{source: source, dialect: id}
	root := source.Document().Root()
	if root.Kind() != oif.Object {
		return r, operations.Invalid("request", "Use a native token-count object.")
	}
	for name, field := range countDefaults(id) {
		if value, present := root.Lookup(name); present {
			if err := field.Validate(value); err != nil {
				return r, operations.Invalid(name, "The control does not match its native schema.")
			}
		}
	}
	switch id {
	case "openai-input-tokens":
		r.scope = "responses-input"
		if model, present := root.Lookup("model"); present && !nullableString(model) {
			return r, operations.Invalid("model", "Use a native model string or null.")
		}
		input := operations.Member(root, "input")
		if !operations.Optional(input) && input.Kind() != oif.String && input.Kind() != oif.Array {
			return r, operations.Invalid("input", "Use native text, input items, or null.")
		}
		if input.Kind() == oif.Array {
			for _, item := range input.Elements() {
				if item.Kind() != oif.Object {
					return r, operations.Invalid("input", "Response input items must be objects.")
				}
			}
		}
		for _, name := range []string{"previous_response_id", "conversation"} {
			if value, present := root.Lookup(name); present && !operations.Optional(value) {
				return r, resourceError("/" + name)
			}
		}
		if err := resourceBlocks(input, "/input"); err != nil {
			return r, err
		}
	case "anthropic-count-tokens":
		r.scope = "messages-system-tools"
		if operations.String(operations.Member(root, "model")) == "" {
			return r, operations.Invalid("model", "Use a native model identity.")
		}
		if err := messages(operations.Member(root, "messages"), "messages", false); err != nil {
			return r, err
		}
		if err := resourceBlocks(operations.Member(root, "messages"), "/messages"); err != nil {
			return r, err
		}
	case "gemini-count-tokens":
		contents, full := operations.Member(root, "contents"), operations.Member(root, "generateContentRequest")
		if !operations.Optional(contents) && !operations.Optional(full) {
			return r, operations.Invalid("generateContentRequest", "Native contents and generateContentRequest are mutually exclusive.")
		}
		r.scope = "contents"
		if !operations.Optional(full) {
			if full.Kind() != oif.Object {
				return r, operations.Invalid("generateContentRequest", "Use a native generate-content object.")
			}
			r.scope = "generate-content-request"
			contents = operations.Member(full, "contents")
			if err := resourceBlocks(operations.Member(full, "systemInstruction"), "/generateContentRequest/systemInstruction"); err != nil {
				return r, err
			}
			if model, present := full.Lookup("model"); present && !nullableString(model) {
				return r, operations.Invalid("generateContentRequest.model", "Use a native model string or null.")
			}
			if cached, present := full.Lookup("cachedContent"); present && !operations.Optional(cached) {
				return r, resourceError("/generateContentRequest/cachedContent")
			}
		}
		if !operations.Optional(contents) {
			if err := geminiContents(contents); err != nil {
				return r, err
			}
		}
		if err := resourceBlocks(contents, "/contents"); err != nil {
			return r, err
		}
	case "bedrock-count-tokens":
		input := operations.Member(root, "input")
		if input.Kind() != oif.Object || len(input.Members()) != 1 {
			return r, operations.Invalid("input", "Choose exactly one native converse or invokeModel input.")
		}
		if converse, present := input.Lookup("converse"); present {
			r.scope = "converse"
			if converse.Kind() != oif.Object {
				return r, operations.Invalid("input.converse", "Use a native converse object.")
			}
			if err := messages(operations.Member(converse, "messages"), "input.converse.messages", true); err != nil {
				return r, err
			}
			if err := resourceBlocks(converse, "/input/converse"); err != nil {
				return r, err
			}
		} else if invoke, present := input.Lookup("invokeModel"); present {
			r.scope = "invoke-model-body"
			body := operations.Member(invoke, "body")
			if invoke.Kind() != oif.Object || body.Kind() != oif.String {
				return r, operations.Invalid("input.invokeModel.body", "Use the native base64 request body.")
			}
			if _, err := base64.StdEncoding.Strict().DecodeString(operations.String(body)); err != nil {
				return r, operations.Invalid("input.invokeModel.body", "The native body is not valid base64.")
			}
		} else {
			return r, operations.Invalid("input", "This count input variant has no native contract.")
		}
	}
	return r, nil
}
func messages(value oif.Value, path string, bedrock bool) error {
	if value.Kind() != oif.Array {
		return operations.Invalid(path, "Use a native message array.")
	}
	for _, message := range value.Elements() {
		role := operations.String(operations.Member(message, "role"))
		content := operations.Member(message, "content")
		validRole := role == "user" || role == "assistant" || (!bedrock && role == "system")
		validContent := content.Kind() == oif.Array || (!bedrock && content.Kind() == oif.String)
		if message.Kind() != oif.Object || !validRole || !validContent {
			return operations.Invalid(path, "Messages require a native role and content representation.")
		}
		if content.Kind() == oif.Array {
			for _, block := range content.Elements() {
				if block.Kind() != oif.Object {
					return operations.Invalid(path, "Content blocks must be native objects.")
				}
			}
		}
	}
	return nil
}
func geminiContents(value oif.Value) error {
	if value.Kind() != oif.Array {
		return operations.Invalid("contents", "Use a native content array.")
	}
	for _, content := range value.Elements() {
		parts := operations.Member(content, "parts")
		if content.Kind() != oif.Object || parts.Kind() != oif.Array {
			return operations.Invalid("contents.parts", "Native contents require a parts array.")
		}
		for _, part := range parts.Elements() {
			if part.Kind() != oif.Object {
				return operations.Invalid("contents.parts", "Native parts must be objects.")
			}
		}
	}
	return nil
}
func resourceError(path string) error {
	return operations.Error("resource_affinity", path, "resource_authority", "This native resource reference requires an authorized serving-bound resolver.")
}

// Only dialect-defined resource carriers are classified; schema property names
// and arbitrary user JSON do not become resources because of their spelling.
func resourceBlocks(value oif.Value, path string) error {
	if value.Kind() == oif.Array {
		for i, item := range value.Elements() {
			if err := resourceBlocks(item, path+"/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		return nil
	}
	if value.Kind() != oif.Object {
		return nil
	}
	kind := operations.String(operations.Member(value, "type"))
	if kind == "item_reference" {
		return resourceError(path)
	}
	if kind == "input_file" || kind == "input_image" {
		if v, ok := value.Lookup("file_id"); ok && !operations.Optional(v) {
			return resourceError(path + "/file_id")
		}
	}
	if source, present := value.Lookup("source"); present && source.Kind() == oif.Object && operations.String(operations.Member(source, "type")) == "file" {
		return resourceError(path + "/source")
	}
	for _, name := range []string{"fileData", "file_data"} {
		if v, present := value.Lookup(name); present && !operations.Optional(v) {
			return resourceError(operations.Pointer(path, name))
		}
	}
	for _, name := range []string{"messages", "content", "parts", "source", "image", "document"} {
		if nested, present := value.Lookup(name); present {
			if err := resourceBlocks(nested, operations.Pointer(path, name)); err != nil {
				return err
			}
		}
	}
	return nil
}
func countField(id string) string {
	switch id {
	case "gemini-count-tokens":
		return "totalTokens"
	case "bedrock-count-tokens":
		return "inputTokens"
	default:
		return "input_tokens"
	}
}
func countResult(request oif.Request, source oif.Result, id string) (oif.View, error) {
	view, err := countRequest(request, id)
	if err != nil {
		return nil, err
	}
	r := CountResult{source: source}
	root := source.Source().Root()
	if root.Kind() != oif.Object {
		return r, operations.Violation("result", "native_token_count")
	}
	name := countField(id)
	value := operations.Member(root, name)
	if _, ok := integer(value, 64); !ok {
		return r, operations.Violation(name, "native_token_count")
	}
	if id == "openai-input-tokens" && operations.String(operations.Member(root, "object")) != "response.input_tokens" {
		return r, operations.Violation("object", "native_count_identity")
	}
	r.counts = append(r.counts, NativeCount{Name: name, Scope: view.(CountRequest).scope, Pointer: "/" + name, Value: value})
	if id == "gemini-count-tokens" {
		if cached, present := root.Lookup("cachedContentTokenCount"); present {
			if _, ok := integer(cached, 64); !ok {
				return r, operations.Violation("cachedContentTokenCount", "native_cached_count")
			}
			r.counts = append(r.counts, NativeCount{Name: "cachedContentTokenCount", Scope: "cached-content", Pointer: "/cachedContentTokenCount", Value: cached})
		}
		for _, field := range []string{"promptTokensDetails", "cacheTokensDetails"} {
			details, present := root.Lookup(field)
			if !present {
				continue
			}
			if details.Kind() != oif.Array {
				return r, operations.Violation(field, "native_modality_counts")
			}
			for i, item := range details.Elements() {
				count := operations.Member(item, "tokenCount")
				if item.Kind() != oif.Object || operations.Member(item, "modality").Kind() != oif.String {
					return r, operations.Violation(field, "native_modality_counts")
				}
				if _, ok := integer(count, 64); !ok {
					return r, operations.Violation(field, "native_modality_counts")
				}
				r.counts = append(r.counts, NativeCount{Name: "tokenCount", Scope: field + ":" + operations.String(operations.Member(item, "modality")), Pointer: "/" + field + "/" + strconv.Itoa(i) + "/tokenCount", Value: count})
			}
		}
	}
	return r, nil
}

// Integer validation is exact and bounded. JSON integer values may have decimal
// or exponent spelling; no float64 conversion participates in acceptance.
func integer(value oif.Value, bits int) (uint64, bool) {
	if value.Kind() != oif.Number {
		return 0, false
	}
	raw := value.Raw()
	if len(raw) > 256 {
		return 0, false
	}
	if p := strings.IndexAny(raw, "eE"); p >= 0 {
		e, err := strconv.Atoi(raw[p+1:])
		if err != nil || e < -1024 || e > 1024 {
			return 0, false
		}
	}
	n, ok := new(big.Rat).SetString(raw)
	if !ok || !n.IsInt() || n.Sign() < 0 || n.Num().BitLen() > bits {
		return 0, false
	}
	return n.Num().Uint64(), true
}
func ValidateRoute(request oif.Request, route string) error {
	if request.Descriptor().Dialect.ID != "gemini-count-tokens" {
		return nil
	}
	full := operations.Member(request.Document().Root(), "generateContentRequest")
	model, present := full.Lookup("model")
	if !present || model.Kind() == oif.Null {
		return nil
	}
	if model.Kind() != oif.String || stripModelPrefix(operations.String(model)) != route {
		return operations.Error("resource_affinity", "/generateContentRequest/model", "model_binding", "The embedded model does not name the selected route.")
	}
	return nil
}

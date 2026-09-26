// Package tokenization preserves native counting and tokenizer results. A native
// count is an operation result, not an estimate or a billed usage observation.
package tokenization

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"slices"
)

var identity = oif.Identity{ID: "token_count", Revision: operations.Revision}

func Identity() oif.Identity { return identity }

type CountRequest struct {
	source         oif.Request
	dialect, scope string
}

func (r CountRequest) Schema() oif.Identity { return identity }
func (r CountRequest) Source() oif.Request  { return r.source }
func (r CountRequest) Scope() string        { return r.scope }

// NativeCount retains its dialect-owned name and scope. Value stays an exact
// source span, including integer tokens outside floating-point precision.
type NativeCount struct {
	Name, Scope, Pointer string
	Value                oif.Value
}
type CountResult struct {
	source oif.Result
	counts []NativeCount
}

func (r CountResult) Schema() oif.Identity  { return identity }
func (r CountResult) Source() oif.Result    { return r.source }
func (r CountResult) Counts() []NativeCount { return slices.Clone(r.counts) }
func (r CountResult) Method() string        { return "provider_native" }

type TokenizeRequest struct {
	source oif.Request
	inputs oif.Value
	batch  bool
	count  int
}

func (r TokenizeRequest) Schema() oif.Identity { return identity }
func (r TokenizeRequest) Source() oif.Request  { return r.source }
func (r TokenizeRequest) Inputs() oif.Value    { return r.inputs }
func (r TokenizeRequest) Batch() bool          { return r.batch }

// NativeToken offsets stay in the provider's coordinate system. The view never
// infers bytes versus code points, strips special tokens or normalizes text.
type NativeToken struct {
	Position                               int
	ID, Text, Special, Start, Stop, Source oif.Value
}
type TokenSet struct {
	InputIndex int
	Source     oif.Value
	tokens     []NativeToken
}

func (s TokenSet) Tokens() []NativeToken { return slices.Clone(s.tokens) }

type TokenizeResult struct {
	source oif.Result
	sets   []TokenSet
}

func (r TokenizeResult) Schema() oif.Identity { return identity }
func (r TokenizeResult) Source() oif.Result   { return r.source }
func (r TokenizeResult) Sets() []TokenSet     { return slices.Clone(r.sets) }
func (r TokenizeResult) Method() string       { return "provider_native" }

func Definitions() []operations.Dialect {
	out := []operations.Dialect{}
	for _, entry := range []struct{ id, surface, path, docs string }{
		{"openai-input-tokens", "openai", "input_tokens", "https://developers.openai.com/api/reference/typescript/resources/responses/subresources/input_tokens/methods/count"},
		{"anthropic-count-tokens", "anthropic", "anthropic_count", "https://platform.claude.com/docs/en/api/typescript/messages/count_tokens"},
		{"gemini-count-tokens", "gemini", "gemini_count", "https://ai.google.dev/api/tokens"},
		{"bedrock-count-tokens", "bedrock", "bedrock_count", "https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_CountTokens.html"},
	} {
		id := entry.id
		d := operations.Dialect{Identity: oif.Identity{ID: id, Revision: operations.Revision}, Operation: identity, Surface: entry.surface, Label: id, Address: operations.Address{FamilyPath: entry.path}, Documentation: entry.docs, Evidence: id + "-native-count-scope/1"}
		d.Request = func(source oif.Request) (oif.View, error) { return countRequest(source, id) }
		d.Result = func(request oif.Request, result oif.Result) (oif.View, error) {
			return countResult(request, result, id)
		}
		d.InputText = func(request oif.Request) ([]operations.Text, error) { return countText(request, id) }
		d.OutputText = outputText
		d.Estimate = estimate
		d.Probe = func(model string) []byte { return countProbe(id, model) }
		d.Defaults = countDefaults(id)
		d.RequestSchema = countRequestSchema(id)
		d.ResultSchema = operations.ObjectSchema(map[string]any{countField(id): map[string]any{"type": "integer", "minimum": 0}}, countField(id))
		if id == "openai-input-tokens" {
			d.ResultSchema = operations.ObjectSchema(map[string]any{"object": map[string]any{"const": "response.input_tokens"}, "input_tokens": map[string]any{"type": "integer", "minimum": 0}}, "object", "input_tokens")
		}
		if id == "openai-input-tokens" || id == "anthropic-count-tokens" {
			d.BindModel = bindModel
		}
		if id == "gemini-count-tokens" {
			d.BindModel = bindGeminiModel
			d.ValidateRoute = ValidateRoute
		}
		out = append(out, d)
	}
	d := operations.Dialect{Identity: oif.Identity{ID: "tei-tokenize", Revision: operations.Revision}, Operation: identity, Surface: "native", Label: "TEI native tokenizer", Address: operations.Address{RelativePath: "tokenize"}, Documentation: "https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/types.rs", Evidence: "tei-tokenize-native-offsets/1", Request: tokenizeRequest, Result: tokenizeResult, InputText: tokenizeText, OutputText: outputText, Estimate: estimate, Probe: func(string) []byte {
		return []byte(`{"inputs":"A neutral tokenizer probe.","add_special_tokens":true}`)
	}}
	d.Defaults = map[string]operations.Field{"add_special_tokens": field(map[string]any{"type": "boolean"}, func(v oif.Value) bool { return v.Kind() == oif.Boolean }), "prompt_name": field(map[string]any{"type": []string{"string", "null"}}, nullableString)}
	d.RequestSchema = operations.ObjectSchema(map[string]any{"inputs": map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}}}, "add_special_tokens": map[string]any{"type": "boolean"}, "prompt_name": map[string]any{"type": []string{"string", "null"}}}, "inputs")
	d.ResultSchema = operations.Raw(map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": map[string]any{"type": "object", "required": []string{"id", "text", "special", "start", "stop"}}}, "description": "One ordered token array per input, retaining native IDs, text, special flags and nullable offsets."})
	return append(out, d)
}
func estimate(view oif.View) int64 {
	var doc oif.Document
	switch r := view.(type) {
	case CountRequest:
		doc = r.source.Document()
	case TokenizeRequest:
		doc = r.source.Document()
	}
	n := int64((len(doc.Raw()) + 3) / 4)
	if n < 1 {
		return 1
	}
	return n
}
func bindModel(doc oif.Document, model string) ([]oif.Change, error) {
	encoded, _ := json.Marshal(model)
	return []oif.Change{{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "published model binding"}}, nil
}
func bindGeminiModel(doc oif.Document, model string) ([]oif.Change, error) {
	nested := operations.Member(doc.Root(), "generateContentRequest")
	if nested.Kind() != oif.Object {
		return nil, nil
	}
	if modelValue, present := nested.Lookup("model"); !present || modelValue.Kind() != oif.String {
		return nil, nil
	}
	encoded, _ := json.Marshal("models/" + stripModelPrefix(model))
	return []oif.Change{{Pointer: "/generateContentRequest/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "published model binding"}}, nil
}
func stripModelPrefix(model string) string {
	if len(model) > 7 && model[:7] == "models/" {
		return model[7:]
	}
	return model
}
func field(schema map[string]any, valid func(oif.Value) bool) operations.Field {
	return operations.FieldSchema(schema, func(v oif.Value) error {
		if !valid(v) {
			return operations.Invalid("default", "The value does not match this native control's schema.")
		}
		return nil
	})
}
func nullableString(v oif.Value) bool { return v.Kind() == oif.String || v.Kind() == oif.Null }
func nullableObject(v oif.Value) bool { return v.Kind() == oif.Object || v.Kind() == oif.Null }
func nullableArray(v oif.Value) bool  { return v.Kind() == oif.Array || v.Kind() == oif.Null }

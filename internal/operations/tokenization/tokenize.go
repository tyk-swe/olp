package tokenization

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func tokenizeRequest(source oif.Request) (oif.View, error) {
	root := source.Document().Root()
	r := TokenizeRequest{source: source, inputs: operations.Member(root, "inputs"), count: 1}
	if root.Kind() != oif.Object {
		return r, operations.Invalid("request", "Use a native tokenize object.")
	}
	switch r.inputs.Kind() {
	case oif.String:
	case oif.Array:
		r.batch = true
		r.count = len(r.inputs.Elements())
		if r.count == 0 {
			return r, operations.Invalid("inputs", "A native tokenizer batch must not be empty.")
		}
		for _, item := range r.inputs.Elements() {
			if item.Kind() != oif.String {
				return r, operations.Invalid("inputs", "Native tokenizer inputs must be strings.")
			}
		}
	default:
		return r, operations.Invalid("inputs", "Use a string or native string batch.")
	}
	if value, present := root.Lookup("add_special_tokens"); present && value.Kind() != oif.Boolean {
		return r, operations.Invalid("add_special_tokens", "Use a native boolean, or omit the control.")
	}
	if value, present := root.Lookup("prompt_name"); present && !nullableString(value) {
		return r, operations.Invalid("prompt_name", "Use a native prompt name or null.")
	}
	return r, nil
}
func tokenizeResult(request oif.Request, source oif.Result) (oif.View, error) {
	view, err := tokenizeRequest(request)
	if err != nil {
		return nil, err
	}
	r := TokenizeResult{source: source}
	root := source.Source().Root()
	if root.Kind() != oif.Array || len(root.Elements()) != view.(TokenizeRequest).count {
		return r, operations.Violation("result", "tokenizer_input_scope")
	}
	for i, set := range root.Elements() {
		if set.Kind() != oif.Array {
			return r, operations.Violation("result", "native_token_sequence")
		}
		out := TokenSet{InputIndex: i, Source: set}
		for j, item := range set.Elements() {
			token := NativeToken{Position: j, ID: operations.Member(item, "id"), Text: operations.Member(item, "text"), Special: operations.Member(item, "special"), Start: operations.Member(item, "start"), Stop: operations.Member(item, "stop"), Source: item}
			if item.Kind() != oif.Object || token.Text.Kind() != oif.String || token.Special.Kind() != oif.Boolean {
				return r, operations.Violation("result", "native_token_fields")
			}
			if _, ok := integer(token.ID, 32); !ok {
				return r, operations.Violation("id", "native_token_identity")
			}
			if token.Start.Kind() == oif.Null && token.Stop.Kind() == oif.Null {
			} else {
				start, sok := integer(token.Start, 64)
				stop, eok := integer(token.Stop, 64)
				if !sok || !eok || stop < start {
					return r, operations.Violation("start", "native_token_offsets")
				}
			}
			out.tokens = append(out.tokens, token)
		}
		r.sets = append(r.sets, out)
	}
	return r, nil
}

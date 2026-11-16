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
	for _, set := range root.Elements() {
		if set.Kind() != oif.Array {
			return r, operations.Violation("result", "native_token_sequence")
		}
		for _, item := range set.Elements() {
			tokenID, startValue, stopValue := operations.Member(item, "id"), operations.Member(item, "start"), operations.Member(item, "stop")
			if item.Kind() != oif.Object || operations.Member(item, "text").Kind() != oif.String || operations.Member(item, "special").Kind() != oif.Boolean {
				return r, operations.Violation("result", "native_token_fields")
			}
			if _, ok := integer(tokenID, 32); !ok {
				return r, operations.Violation("id", "native_token_identity")
			}
			if startValue.Kind() == oif.Null && stopValue.Kind() == oif.Null {
			} else {
				start, sok := integer(startValue, 64)
				stop, eok := integer(stopValue, 64)
				if !sok || !eok || stop < start {
					return r, operations.Violation("start", "native_token_offsets")
				}
			}
		}
	}
	return r, nil
}

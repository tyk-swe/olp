package classification

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func predictRequest(source oif.Request) (oif.View, error) {
	root := source.Document().Root()
	r := PredictRequest{source: source}
	if root.Kind() != oif.Object {
		return r, operations.Invalid("request", "Use a native prediction object.")
	}
	if err := validateFields(root, map[string]operations.Field{"raw_scores": boolean(), "truncate": nullableBool(), "truncation_direction": direction()}); err != nil {
		return r, err
	}
	inputs := operations.Member(root, "inputs")
	if inputs.Kind() == oif.String {
		r.count = 1
		return r, nil
	}
	if inputs.Kind() != oif.Array || len(inputs.Elements()) == 0 {
		return r, operations.Invalid("inputs", "Use a single string, a single pair, or a nested batch.")
	}
	items := inputs.Elements()
	if items[0].Kind() == oif.String {
		r.count = 1
		return r, validSequence(inputs)
	}
	r.batch = true
	r.count = len(items)
	for _, item := range items {
		if err := validSequence(item); err != nil {
			return r, err
		}
	}
	return r, nil
}
func validSequence(value oif.Value) error {
	items := value.Elements()
	if value.Kind() != oif.Array || len(items) < 1 || len(items) > 2 {
		return operations.Invalid("inputs", "Each sequence must contain one or two strings.")
	}
	for _, item := range items {
		if item.Kind() != oif.String {
			return operations.Invalid("inputs", "Sequence elements must be strings.")
		}
	}
	return nil
}
func predictResult(request oif.Request, source oif.Result) (oif.View, error) {
	view, err := predictRequest(request)
	if err != nil {
		return nil, err
	}
	req := view.(PredictRequest)
	r := PredictResult{source: source}
	root := source.Source().Root()
	if root.Kind() != oif.Array {
		return r, operations.Violation("result", "prediction_scope")
	}
	sets := []oif.Value{root}
	if req.batch {
		sets = root.Elements()
		if len(sets) != req.count {
			return r, operations.Violation("result", "prediction_batch_scope")
		}
	}
	for _, set := range sets {
		if set.Kind() != oif.Array || len(set.Elements()) == 0 {
			return r, operations.Violation("result", "prediction_set")
		}
		for _, item := range set.Elements() {
			label, score := operations.Member(item, "label"), operations.Member(item, "score")
			if item.Kind() != oif.Object || label.Kind() != oif.String || score.Kind() != oif.Number {
				return r, operations.Violation("result", "native_prediction")
			}
		}
	}
	return r, nil
}

package classification

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func predictRequest(source oif.Request) (oif.View, error) {
	root := source.Document().Root()
	r := PredictRequest{source: source, rawScores: operations.Member(root, "raw_scores")}
	if root.Kind() != oif.Object {
		return r, operations.Invalid("request", "Use a native prediction object.")
	}
	if err := validateFields(root, map[string]operations.Field{"raw_scores": boolean(), "truncate": nullableBool(), "truncation_direction": direction()}); err != nil {
		return r, err
	}
	inputs := operations.Member(root, "inputs")
	if inputs.Kind() == oif.String {
		r.sequences = []Sequence{{Source: inputs}}
		return r, nil
	}
	if inputs.Kind() != oif.Array || len(inputs.Elements()) == 0 {
		return r, operations.Invalid("inputs", "Use a single string, a single pair, or a nested batch.")
	}
	items := inputs.Elements()
	if items[0].Kind() == oif.String {
		sequence, err := sequence(inputs)
		r.sequences = []Sequence{sequence}
		return r, err
	}
	r.batch = true
	for _, item := range items {
		seq, err := sequence(item)
		if err != nil {
			return r, err
		}
		r.sequences = append(r.sequences, seq)
	}
	return r, nil
}
func sequence(value oif.Value) (Sequence, error) {
	r := Sequence{Source: value}
	items := value.Elements()
	if value.Kind() != oif.Array || len(items) < 1 || len(items) > 2 {
		return r, operations.Invalid("inputs", "Each sequence must contain one or two strings.")
	}
	for _, item := range items {
		if item.Kind() != oif.String {
			return r, operations.Invalid("inputs", "Sequence elements must be strings.")
		}
	}
	r.Pair = len(items) == 2
	return r, nil
}
func predictResult(request oif.Request, source oif.Result) (oif.View, error) {
	view, err := predictRequest(request)
	if err != nil {
		return nil, err
	}
	req := view.(PredictRequest)
	r := PredictResult{source: source, batch: req.batch, rawScores: req.rawScores}
	root := source.Source().Root()
	if root.Kind() != oif.Array {
		return r, operations.Violation("result", "prediction_scope")
	}
	sets := []oif.Value{root}
	if req.batch {
		sets = root.Elements()
		if len(sets) != len(req.sequences) {
			return r, operations.Violation("result", "prediction_batch_scope")
		}
	}
	for inputIndex, set := range sets {
		if set.Kind() != oif.Array || len(set.Elements()) == 0 {
			return r, operations.Violation("result", "prediction_set")
		}
		out := PredictionSet{InputIndex: inputIndex, Source: set}
		for position, item := range set.Elements() {
			label, score := operations.Member(item, "label"), operations.Member(item, "score")
			if item.Kind() != oif.Object || label.Kind() != oif.String || score.Kind() != oif.Number {
				return r, operations.Violation("result", "native_prediction")
			}
			out.rows = append(out.rows, Prediction{Position: position, Label: label, Score: score, Source: item})
		}
		r.sets = append(r.sets, out)
	}
	return r, nil
}

package classification

import (
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

func validateParameters(value oif.Value) error {
	if operations.Optional(value) {
		return nil
	}
	if value.Kind() != oif.Object {
		return operations.Invalid("parameters", "Use a native parameter object or null.")
	}
	if err := validateFields(value, map[string]operations.Field{"truncate": nullableBool(), "truncation_direction": direction()}); err != nil {
		return err
	}
	if prompt, present := value.Lookup("prompt_name"); present && !operations.Optional(prompt) && prompt.Kind() != oif.String {
		return operations.Invalid("prompt_name", "Use a native prompt name or null.")
	}
	return nil
}
func scoringRequest(source oif.Request) (oif.View, error) {
	root := source.Document().Root()
	input := operations.Member(root, "inputs")
	sourceSentence := operations.Member(input, "source_sentence")
	parameters := operations.Member(root, "parameters")
	r := ScoringRequest{source: source, sentences: operations.Member(input, "sentences")}
	if root.Kind() != oif.Object || input.Kind() != oif.Object || sourceSentence.Kind() != oif.String || r.sentences.Kind() != oif.Array || len(r.sentences.Elements()) == 0 {
		return r, operations.Invalid("inputs", "Use a source sentence and a non-empty sentence collection.")
	}
	for _, sentence := range r.sentences.Elements() {
		if sentence.Kind() != oif.String {
			return r, operations.Invalid("inputs.sentences", "Every comparison sentence must be a string.")
		}
	}
	return r, validateParameters(parameters)
}
func scoringResult(request oif.Request, source oif.Result) (oif.View, error) {
	view, err := scoringRequest(request)
	if err != nil {
		return nil, err
	}
	r := ScoringResult{source: source}
	scores := source.Source().Root()
	if scores.Kind() != oif.Array || len(scores.Elements()) != len(view.(ScoringRequest).sentences.Elements()) {
		return r, operations.Violation("result", "similarity_set_scope")
	}
	for _, score := range scores.Elements() {
		if score.Kind() != oif.Number {
			return r, operations.Violation("result", "native_similarity_score")
		}
	}
	return r, nil
}

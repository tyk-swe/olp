// Package classification retains operation-owned moderation, classification and
// similarity views. Native scores and labels never become a normalized map.
package classification

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const teiDocs = "https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/types.rs"

func identity(operation string) oif.Identity {
	return oif.Identity{ID: operation, Revision: operations.Revision}
}

// Scope distinguishes a single text, a text batch, and a joint multimodal input.
// Count is native result cardinality, never a license to split the request.
type Scope struct {
	Kind  string
	Count int
}
type Sequence struct {
	Source oif.Value
	Pair   bool
}

type ModerationRequest struct {
	source oif.Request
	scope  Scope
}

func (r ModerationRequest) Schema() oif.Identity { return identity("moderation") }
func (r ModerationRequest) Source() oif.Request  { return r.source }
func (r ModerationRequest) Scope() Scope         { return r.scope }

type Decision struct {
	Position                                                   int
	Categories, Scores, Flagged, AppliedInputTypes, Thresholds oif.Value
}
type ModerationResult struct {
	source    oif.Result
	scope     Scope
	decisions []Decision
}

func (r ModerationResult) Schema() oif.Identity  { return identity("moderation") }
func (r ModerationResult) Source() oif.Result    { return r.source }
func (r ModerationResult) Scope() Scope          { return r.scope }
func (r ModerationResult) Decisions() []Decision { return slices.Clone(r.decisions) }

type PredictRequest struct {
	source    oif.Request
	batch     bool
	sequences []Sequence
	rawScores oif.Value
}

func (r PredictRequest) Schema() oif.Identity  { return identity("classification") }
func (r PredictRequest) Source() oif.Request   { return r.source }
func (r PredictRequest) Batch() bool           { return r.batch }
func (r PredictRequest) Sequences() []Sequence { return slices.Clone(r.sequences) }
func (r PredictRequest) RawScores() oif.Value  { return r.rawScores }

type Prediction struct {
	Position             int
	Label, Score, Source oif.Value
}
type PredictionSet struct {
	InputIndex int
	Source     oif.Value
	rows       []Prediction
}

func (s PredictionSet) Predictions() []Prediction { return slices.Clone(s.rows) }

type PredictResult struct {
	source    oif.Result
	batch     bool
	rawScores oif.Value
	sets      []PredictionSet
}

func (r PredictResult) Schema() oif.Identity  { return identity("classification") }
func (r PredictResult) Source() oif.Result    { return r.source }
func (r PredictResult) Batch() bool           { return r.batch }
func (r PredictResult) RawScores() oif.Value  { return r.rawScores }
func (r PredictResult) Sets() []PredictionSet { return slices.Clone(r.sets) }

type ScoringRequest struct {
	source                                oif.Request
	sourceSentence, sentences, parameters oif.Value
}

func (r ScoringRequest) Schema() oif.Identity      { return identity("scoring") }
func (r ScoringRequest) Source() oif.Request       { return r.source }
func (r ScoringRequest) SourceSentence() oif.Value { return r.sourceSentence }
func (r ScoringRequest) Sentences() oif.Value      { return r.sentences }
func (r ScoringRequest) Parameters() oif.Value     { return r.parameters }

type Similarity struct {
	InputIndex int
	Score      oif.Value
}
type ScoringResult struct {
	source oif.Result
	scores []Similarity
}

func (r ScoringResult) Schema() oif.Identity { return identity("scoring") }
func (r ScoringResult) Source() oif.Result   { return r.source }
func (r ScoringResult) Scores() []Similarity { return slices.Clone(r.scores) }

func Definitions() []operations.Dialect {
	moderation := operations.Dialect{Identity: identity("openai-moderation"), Operation: identity("moderation"), Surface: "openai", Label: "OpenAI native moderation", Address: operations.Address{LegacyPath: "moderation"}, Documentation: "https://developers.openai.com/api/reference/typescript/resources/moderations/methods/create", Evidence: "openai-moderation-native-scope/1", Request: moderationRequest, Result: moderationResult, BindModel: bindModel, ModelBinding: operations.ModelRequired, BindResultModel: operations.ModelChanges, InputText: moderationText, OutputText: outputText, Probe: func(model string) []byte {
		return operations.Raw(map[string]any{"model": model, "input": "A neutral moderation probe."})
	}}
	moderation.RequestSchema = operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}, "input": map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": []string{"string", "object"}}}}}}, "input")
	moderation.ResultSchema = operations.ObjectSchema(map[string]any{"id": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "results": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "required": []string{"flagged", "categories", "category_scores"}}}}, "id", "model", "results")
	predict := operations.Dialect{Identity: identity("tei-classification"), Operation: identity("classification"), Surface: "native", Label: "TEI native classification", Address: operations.Address{RelativePath: "predict"}, Documentation: teiDocs, Evidence: "tei-predict-pair-batch-scores/1", Request: predictRequest, Result: predictResult, InputText: predictText, OutputText: outputText, Probe: func(string) []byte { return []byte(`{"inputs":"A neutral classification probe.","raw_scores":true}`) }}
	predict.Defaults = map[string]operations.Field{"truncate": nullableBool(), "raw_scores": boolean(), "truncation_direction": direction()}
	predict.RequestSchema = operations.ObjectSchema(map[string]any{"inputs": map[string]any{"description": "Scalar, one/two-string single sequence, or nonempty nested batch of one/two-string sequences.", "type": []string{"string", "array"}}, "truncate": map[string]any{"type": []string{"boolean", "null"}}, "raw_scores": map[string]any{"type": "boolean"}, "truncation_direction": map[string]any{"enum": []string{"Left", "left", "Right", "right"}}}, "inputs")
	predict.ResultSchema = operations.Raw(map[string]any{"type": "array", "description": "Prediction records for a single sequence, or one prediction-record array per batch input; exact native scores and ordered labels."})
	scoring := operations.Dialect{Identity: identity("tei-scoring"), Operation: identity("scoring"), Surface: "native", Label: "TEI native similarity", Address: operations.Address{RelativePath: "similarity"}, Documentation: teiDocs, Evidence: "tei-similarity-set-scores/1", Request: scoringRequest, Result: scoringResult, InputText: scoringText, OutputText: outputText, Probe: func(string) []byte {
		return []byte(`{"inputs":{"source_sentence":"source","sentences":["first","second"]}}`)
	}}
	scoring.Defaults = map[string]operations.Field{"parameters": operations.FieldSchema(map[string]any{"type": []string{"object", "null"}}, validateParameters)}
	scoring.RequestSchema = operations.ObjectSchema(map[string]any{"inputs": map[string]any{"type": "object", "required": []string{"source_sentence", "sentences"}, "properties": map[string]any{"source_sentence": map[string]any{"type": "string"}, "sentences": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}}}, "parameters": map[string]any{"type": []string{"object", "null"}}}, "inputs")
	scoring.ResultSchema = operations.Raw(map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "description": "One unchanged similarity score per input sentence, in original order."})
	for _, d := range []*operations.Dialect{&moderation, &predict, &scoring} {
		d.Estimate = estimate
	}
	return []operations.Dialect{moderation, predict, scoring}
}
func bindModel(doc oif.Document, model string) ([]oif.Change, error) {
	if doc.Root().Kind() != oif.Object {
		return nil, operations.Invalid("model", "The native request must be an object.")
	}
	encoded, _ := json.Marshal(model)
	return []oif.Change{{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "published model binding"}}, nil
}
func estimate(view oif.View) int64 {
	var source oif.Request
	switch r := view.(type) {
	case ModerationRequest:
		source = r.source
	case PredictRequest:
		source = r.source
	case ScoringRequest:
		source = r.source
	}
	return int64((len(source.Document().Raw()) + 3) / 4)
}
func boolean() operations.Field {
	return operations.FieldSchema(map[string]any{"type": "boolean"}, func(v oif.Value) error {
		if v.Kind() != oif.Boolean {
			return operations.Invalid("default", "Use a native boolean.")
		}
		return nil
	})
}
func nullableBool() operations.Field {
	return operations.FieldSchema(map[string]any{"type": []string{"boolean", "null"}}, operations.NullableBool)
}
func direction() operations.Field {
	return operations.FieldSchema(map[string]any{"type": "string", "enum": []string{"Left", "left", "Right", "right"}}, func(v oif.Value) error {
		if !slices.Contains([]string{"Left", "left", "Right", "right"}, operations.String(v)) {
			return operations.Invalid("truncation_direction", "Use a documented native truncation direction.")
		}
		return nil
	})
}
func validateFields(root oif.Value, fields map[string]operations.Field) error {
	for name, field := range fields {
		if value, present := root.Lookup(name); present {
			if err := field.Validate(value); err != nil {
				return operations.Invalid(name, "The control does not match its native schema.")
			}
		}
	}
	return nil
}

package guardrails

import (
	"encoding/json"
	"math"
	"slices"
	"unicode/utf8"
)

const (
	MaxSemanticReferences = 64
	MaxSemanticSegments   = 128
	MaxSemanticDimensions = 4096
)

// SemanticRule compares each effective content segment with known undesirable
// utterances. References and content are embedded in one accounted request so
// provider failover cannot accidentally mix embedding spaces.
type SemanticRule struct {
	Category            string   `json:"category"`
	SimilarityThreshold float64  `json:"similarity_threshold"`
	Utterances          []string `json:"utterances"`
}

func ValidateSemantic(rule *SemanticRule) error {
	if rule == nil || !categoryID.MatchString(rule.Category) || math.IsNaN(rule.SimilarityThreshold) || math.IsInf(rule.SimilarityThreshold, 0) || rule.SimilarityThreshold <= 0 || rule.SimilarityThreshold > 1 || len(rule.Utterances) == 0 || len(rule.Utterances) > MaxSemanticReferences {
		return errEvaluation
	}
	size := 0
	for _, utterance := range rule.Utterances {
		if utterance == "" || len(utterance) > 8192 || !utf8.ValidString(utterance) {
			return errEvaluation
		}
		size += len(utterance)
	}
	if size > 64<<10 {
		return errEvaluation
	}
	return nil
}

// SemanticInputs places nonempty queried segments first, followed by references.
// It returns no inputs when there is no text to evaluate. The response indexes
// are checked against this exact order by SemanticVerdict.
func SemanticInputs(e Evaluation, rule SemanticRule) ([]string, error) {
	if ValidateSemantic(&rule) != nil || validateSegments(e.Segments) != nil || len(e.Segments) > MaxSemanticSegments {
		return nil, errEvaluation
	}
	inputs := make([]string, 0, len(e.Segments)+len(rule.Utterances))
	size := 0
	for _, segment := range e.Segments {
		if segment.Text == "" {
			continue
		}
		inputs = append(inputs, segment.Text)
		size += len(segment.Text)
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	for _, utterance := range rule.Utterances {
		inputs = append(inputs, utterance)
		size += len(utterance)
	}
	if size > MaxContentBytes {
		return nil, errEvaluation
	}
	return inputs, nil
}

// SemanticVerdict validates the entire embedding result before comparing any
// vectors. A malformed later item cannot be hidden behind an earlier match.
func SemanticVerdict(raw []byte, rule SemanticRule, segmentCount int) (Verdict, error) {
	if ValidateSemantic(&rule) != nil || segmentCount < 1 || segmentCount > MaxSemanticSegments {
		return Verdict{}, errEvaluation
	}
	var response struct {
		Data []struct {
			Index     *int       `json:"index"`
			Embedding []*float64 `json:"embedding"`
		} `json:"data"`
	}
	total := segmentCount + len(rule.Utterances)
	if json.Unmarshal(raw, &response) != nil || len(response.Data) != total {
		return Verdict{}, errEvaluation
	}
	vectors := make([][]float64, total)
	dimension := 0
	for _, item := range response.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= total || vectors[*item.Index] != nil || len(item.Embedding) == 0 || len(item.Embedding) > MaxSemanticDimensions {
			return Verdict{}, errEvaluation
		}
		if dimension == 0 {
			dimension = len(item.Embedding)
		} else if dimension != len(item.Embedding) {
			return Verdict{}, errEvaluation
		}
		vector := make([]float64, dimension)
		largest := 0.0
		for i, value := range item.Embedding {
			if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
				return Verdict{}, errEvaluation
			}
			vector[i] = *value
			largest = math.Max(largest, math.Abs(*value))
		}
		if largest == 0 {
			return Verdict{}, errEvaluation
		}
		normSquared := 0.0
		for i := range vector {
			vector[i] /= largest
			normSquared += vector[i] * vector[i]
		}
		norm := math.Sqrt(normSquared)
		for i := range vector {
			vector[i] /= norm
		}
		vectors[*item.Index] = vector
	}
	for _, query := range vectors[:segmentCount] {
		for _, reference := range vectors[segmentCount:] {
			// Identical normalized directions have cosine 1, even when summing
			// their squared components rounds slightly below that boundary.
			if slices.Equal(query, reference) {
				return vendorBoolean(true, rule.Category), nil
			}
			similarity := 0.0
			for i, value := range query {
				similarity += value * reference[i]
			}
			if similarity >= rule.SimilarityThreshold {
				return vendorBoolean(true, rule.Category), nil
			}
		}
	}
	return vendorBoolean(false, rule.Category), nil
}

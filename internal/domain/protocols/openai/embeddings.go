package openai

import (
	"encoding/json"
	"slices"
)

func validEmbeddingInput(raw json.RawMessage) bool {
	var text *string
	if json.Unmarshal(raw, &text) == nil {
		return text != nil
	}
	var texts []*string
	if json.Unmarshal(raw, &texts) == nil {
		return len(texts) > 0 && !slices.Contains(texts, nil)
	}
	var tokens []*uint32
	if json.Unmarshal(raw, &tokens) == nil {
		return len(tokens) > 0 && !slices.Contains(tokens, nil)
	}
	var batches [][]*uint32
	if json.Unmarshal(raw, &batches) != nil || len(batches) == 0 {
		return false
	}
	for _, tokens := range batches {
		if len(tokens) == 0 || slices.Contains(tokens, nil) {
			return false
		}
	}
	return true
}

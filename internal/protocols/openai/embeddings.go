package openai

import "encoding/json"

func validEmbeddingInput(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return true
	}
	var texts []string
	if json.Unmarshal(raw, &texts) == nil {
		return len(texts) > 0
	}
	var tokens []uint32
	if json.Unmarshal(raw, &tokens) == nil {
		return len(tokens) > 0
	}
	var batches [][]uint32
	if json.Unmarshal(raw, &batches) != nil || len(batches) == 0 {
		return false
	}
	for _, tokens := range batches {
		if len(tokens) == 0 {
			return false
		}
	}
	return true
}

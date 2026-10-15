package protocols

import (
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func textInputs(raw json.RawMessage) ([]string, error) {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return []string{single}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
		return nil, requestError("input", "Native embeddings require a non-empty string or string array input")
	}
	return list, nil
}

func embeddingFields(f Object, extra ...string) error {
	allowed := map[string]bool{"model": true, "input": true, "dimensions": true, "encoding_format": true}
	for _, name := range extra {
		allowed[name] = true
	}
	for name, value := range f {
		if !allowed[name] && present(value) {
			return requestError(name, "The selected provider cannot represent "+name)
		}
	}
	return nil
}

func contentPart(text string) json.RawMessage {
	return raw(map[string]any{"parts": []any{map[string]any{"text": text}}})
}

func encodeNativeEmbeddings(encodedWire wire.Format, f Object, model string) ([]byte, wire.Format, error) {
	inputs, err := textInputs(f["input"])
	if err != nil {
		return nil, encodedWire, err
	}
	dimensions := f["dimensions"]
	model = strings.TrimPrefix(model, "models/")
	switch encodedWire {
	case wire.GeminiEmbeddings:
		if err := embeddingFields(f); err != nil {
			return nil, encodedWire, err
		}
		if len(inputs) > 100 {
			return nil, encodedWire, requestError("input", "Gemini embeddings accept at most 100 inputs")
		}
		if len(inputs) == 1 {
			body := Object{"content": contentPart(inputs[0])}
			if present(dimensions) {
				body["outputDimensionality"] = dimensions
			}
			encoded, err := json.Marshal(body)
			return encoded, encodedWire, err
		}
		requests := make([]Object, len(inputs))
		for i, text := range inputs {
			entry := Object{"model": raw("models/" + model), "content": contentPart(text)}
			if present(dimensions) {
				entry["outputDimensionality"] = dimensions
			}
			requests[i] = entry
		}
		encoded, err := json.Marshal(Object{"requests": raw(requests)})
		return encoded, wire.GeminiEmbeddingsBatch, err
	case wire.VertexEmbeddings:
		if err := embeddingFields(f, "truncation"); err != nil {
			return nil, encodedWire, err
		}
		if len(inputs) > 5 {
			return nil, encodedWire, requestError("input", "Vertex embeddings accept at most 5 inputs")
		}
		instances := make([]Object, len(inputs))
		for i, text := range inputs {
			instances[i] = Object{"content": raw(text)}
		}
		body := Object{"instances": raw(instances)}
		parameters := Object{}
		if present(dimensions) {
			parameters["outputDimensionality"] = dimensions
		}
		if present(f["truncation"]) {
			parameters["autoTruncate"] = f["truncation"]
		}
		if len(parameters) > 0 {
			body["parameters"] = raw(parameters)
		}
		encoded, err := json.Marshal(body)
		return encoded, encodedWire, err
	case wire.BedrockEmbeddings:
		if err := embeddingFields(f, "normalize"); err != nil {
			return nil, encodedWire, err
		}
		if !strings.HasPrefix(model, "amazon.titan-embed-text-") {
			return nil, encodedWire, requestError("model", "Bedrock embeddings are qualified for Titan Embed Text models only")
		}
		if len(inputs) != 1 {
			return nil, encodedWire, requestError("input", "Bedrock embeddings accept exactly one text input")
		}
		body := Object{"inputText": raw(inputs[0])}
		if present(dimensions) {
			body["dimensions"] = dimensions
		}
		if present(f["normalize"]) {
			body["normalize"] = f["normalize"]
		}
		encoded, err := json.Marshal(body)
		return encoded, encodedWire, err
	}
	return nil, encodedWire, requestError("operation", "The selected provider does not support embeddings")
}

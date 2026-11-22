package protocols

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"

	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func embeddingItems(data []json.RawMessage, encoding string) ([]json.RawMessage, error) {
	seen := map[int64]bool{}
	for i, v := range data {
		item, e := object(v)
		if e != nil {
			return nil, protocolError("invalid embedding")
		}
		index, ok := count(item["index"])
		if !ok || seen[index] || index >= int64(len(data)) {
			return nil, protocolError("invalid embedding index")
		}
		seen[index] = true
		var values []float64
		var encoded string
		if json.Unmarshal(item["embedding"], &encoded) == nil {
			b, e := base64.StdEncoding.DecodeString(encoded)
			if e != nil || len(b) == 0 || len(b)%4 != 0 {
				return nil, protocolError("invalid base64 embedding")
			}
			for pos := 0; pos < len(b); pos += 4 {
				v := math.Float32frombits(binary.LittleEndian.Uint32(b[pos:]))
				if math.IsInf(float64(v), 0) || math.IsNaN(float64(v)) {
					return nil, protocolError("non-finite embedding")
				}
				values = append(values, float64(v))
			}
		} else {
			var err error
			values, err = floatVector(item["embedding"])
			if err != nil {
				return nil, err
			}
		}
		for _, value := range values {
			if math.IsInf(value, 0) || math.IsNaN(value) {
				return nil, protocolError("non-finite embedding")
			}
		}
		if encoding == "float" {
			item["embedding"] = raw(values)
		}
		if encoding == "base64" && encoded == "" {
			bytes := make([]byte, 4*len(values))
			for i, value := range values {
				converted := float32(value)
				if math.IsInf(float64(converted), 0) {
					return nil, protocolError("embedding exceeds float32 range")
				}
				binary.LittleEndian.PutUint32(bytes[i*4:], math.Float32bits(converted))
			}
			item["embedding"] = raw(base64.StdEncoding.EncodeToString(bytes))
		}
		item["object"] = raw("embedding")
		data[i] = raw(item)
	}
	return data, nil
}

func nativeEmbeddingsCompletion(values [][]float64, route, encoding string, usage *wire.Usage) (*wire.Completion, error) {
	data := make([]json.RawMessage, len(values))
	for i, vector := range values {
		data[i] = raw(Object{"index": raw(i), "embedding": raw(vector)})
	}
	checked, err := embeddingItems(data, encoding)
	if err != nil {
		return nil, err
	}
	f := Object{"data": raw(checked), "model": raw(route), "object": raw("list")}
	if usage != nil {
		f["usage"] = raw(Object{"prompt_tokens": raw(usage.InputTokens), "total_tokens": raw(usage.TotalTokens)})
	}
	c := &wire.Completion{Body: raw(f), FinishReason: "stop", Usage: usage}
	return c, nil
}

func floatVector(raw json.RawMessage) ([]float64, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil || len(elements) == 0 {
		return nil, protocolError("invalid embedding vector")
	}
	values := make([]float64, len(elements))
	for index, element := range elements {
		if !present(element) || json.Unmarshal(element, &values[index]) != nil {
			return nil, protocolError("invalid embedding vector")
		}
	}
	return values, nil
}

func validateEmbeddingCount(request *wire.Request, actual int) error {
	if request == nil {
		return nil
	}
	var expected int
	switch request.Family {
	case wire.Embeddings:
		count, ok := embeddingInputCount(request.Field("input"))
		if !ok {
			return nil
		}
		expected = count
	case wire.GeminiEmbeddingsBatch:
		if len(request.Field("requests")) == 0 {
			return nil
		}
		expected = len(arr(request.Field("requests")))
	case wire.VertexEmbeddings:
		if len(request.Field("instances")) == 0 {
			return nil
		}
		expected = len(arr(request.Field("instances")))
	case wire.GeminiEmbeddings, wire.BedrockEmbeddings:
		expected = 1
	default:
		return nil
	}
	if expected != actual {
		return protocolError("embedding count does not match input count")
	}
	return nil
}

func decodeGeminiEmbedding(body []byte, route, encoding string) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	embedding, e := optionalObject(f["embedding"])
	if e != nil {
		return nil, e
	}
	values, err := floatVector(embedding["values"])
	if err != nil {
		return nil, err
	}
	usage, err := geminiEmbeddingUsage(f)
	if err != nil {
		return nil, err
	}
	return nativeEmbeddingsCompletion([][]float64{values}, route, encoding, usage)
}

// geminiEmbeddingUsage is the usage a Gemini embedding response reports in its
// usageMetadata: the tokens of the prompt. A response that reports none has no
// usage.
func geminiEmbeddingUsage(f Object) (*wire.Usage, error) {
	metadata, e := optionalObject(f["usageMetadata"])
	if e != nil {
		return nil, e
	}
	if !present(metadata["promptTokenCount"]) {
		return nil, nil
	}
	n, ok := count(metadata["promptTokenCount"])
	if !ok {
		return nil, protocolError("invalid embeddings usage")
	}
	return &wire.Usage{InputTokens: n, TotalTokens: n}, nil
}

func decodeGeminiEmbeddingBatch(body []byte, route, encoding string, request *wire.Request) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	embeddings := arr(f["embeddings"])
	if len(embeddings) == 0 {
		return nil, protocolError("missing embeddings")
	}
	if err := validateEmbeddingCount(request, len(embeddings)); err != nil {
		return nil, err
	}
	values := make([][]float64, len(embeddings))
	for i, v := range embeddings {
		item, e := object(v)
		if e != nil {
			return nil, protocolError("invalid embedding")
		}
		vector, err := floatVector(item["values"])
		if err != nil {
			return nil, err
		}
		values[i] = vector
	}
	usage, err := geminiEmbeddingUsage(f)
	if err != nil {
		return nil, err
	}
	return nativeEmbeddingsCompletion(values, route, encoding, usage)
}

func decodeVertexEmbeddings(body []byte, route, encoding string, request *wire.Request) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	predictions := arr(f["predictions"])
	if len(predictions) == 0 {
		return nil, protocolError("missing embeddings")
	}
	if err := validateEmbeddingCount(request, len(predictions)); err != nil {
		return nil, err
	}
	values := make([][]float64, len(predictions))
	var tokens int64
	var usage *wire.Usage
	for i, v := range predictions {
		prediction, e := object(v)
		if e != nil {
			return nil, protocolError("invalid embedding")
		}
		embedding, e := optionalObject(prediction["embeddings"])
		if e != nil {
			return nil, e
		}
		vector, err := floatVector(embedding["values"])
		if err != nil {
			return nil, err
		}
		values[i] = vector
		if statistics, e := optionalObject(embedding["statistics"]); e != nil {
			return nil, e
		} else if present(statistics["token_count"]) {
			n, ok := count(statistics["token_count"])
			if !ok {
				return nil, protocolError("invalid embeddings usage")
			}
			tokens += n
			usage = &wire.Usage{InputTokens: tokens, TotalTokens: tokens}
		}
	}
	return nativeEmbeddingsCompletion(values, route, encoding, usage)
}

func decodeBedrockEmbedding(body []byte, route, encoding string) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["message"]) && !present(f["embedding"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	values, err := floatVector(f["embedding"])
	if err != nil {
		return nil, err
	}
	n, ok := count(f["inputTextTokenCount"])
	if !ok {
		return nil, protocolError("invalid embeddings usage")
	}
	usage := &wire.Usage{InputTokens: n, TotalTokens: n}
	return nativeEmbeddingsCompletion([][]float64{values}, route, encoding, usage)
}

func decodeEmbeddings(body []byte, route, encoding string, request *wire.Request) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	data := arr(f["data"])
	if len(data) == 0 {
		return nil, protocolError("missing embeddings")
	}
	if err := validateEmbeddingCount(request, len(data)); err != nil {
		return nil, err
	}
	data, err := embeddingItems(data, encoding)
	if err != nil {
		return nil, err
	}
	f["data"] = raw(data)
	f["model"] = raw(route)
	f["object"] = raw("list")
	c := &wire.Completion{Body: raw(f), FinishReason: "stop"}
	usage, e := optionalObject(f["usage"])
	if e != nil {
		return nil, e
	}
	if usage != nil {
		n, ok := count(usage["prompt_tokens"])
		if !ok {
			n, ok = count(usage["total_tokens"])
		}
		if !ok {
			return nil, protocolError("invalid embeddings usage")
		}
		c.Usage = &wire.Usage{InputTokens: n, TotalTokens: n}
	}
	return c, nil
}

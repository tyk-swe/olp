package embeddings

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

func liftResult(source oif.Request, result oif.Result, id string) (Result, error) {
	request, err := liftRequest(source, id)
	out := Result{source: result}
	if err != nil {
		return out, err
	}
	values, indexed, usage, err := extractEmbeddingResult(source, result.Source().Root(), request, id)
	if err != nil {
		return out, err
	}
	out.usage = usage
	if request.format.Layout != "native-typed" && len(values) != request.inputs {
		return out, operations.Violation("/", "input_vector_correspondence")
	}
	seen := map[int]bool{}
	for position, value := range values {
		index := position
		if indexed {
			n, ok := operations.Position(operations.Member(value, "index"))
			if !ok || n >= request.inputs || seen[n] {
				return out, operations.Violation("/data/index", "input_identity")
			}
			index = n
			seen[index] = true
			value = operations.Member(value, "embedding")
		}
		format := request.format
		if id == "gemini-batch-embeddings" {
			entry := operations.Member(source.Document().Root(), "requests").Elements()[index]
			dim := operations.Member(entry, "outputDimensionality")
			if !operations.Optional(dim) {
				format.LogicalDimensions, _ = operations.Int(dim)
				format.DimensionsKnown = true
			}
		}
		if _, err := validateVector(value, format, index); err != nil {
			return out, err
		}
	}
	return out, nil
}

func extractEmbeddingResult(source oif.Request, root oif.Value, request Request, id string) ([]oif.Value, bool, *operations.Usage, error) {
	switch id {
	case "openai-embeddings", "voyage-embeddings":
		data := operations.Member(root, "data")
		if data.Kind() != oif.Array {
			return nil, false, nil, operations.Violation("/data", "embedding_collection")
		}
		var usage *operations.Usage
		if raw, present := root.Lookup("usage"); present {
			var err error
			if usage, err = tokenUsage(raw, id == "voyage-embeddings"); err != nil {
				return nil, false, nil, err
			}
		}
		return data.Elements(), true, usage, nil
	case "gemini-embeddings":
		usage, err := geminiUsage(root)
		return []oif.Value{operations.Member(operations.Member(root, "embedding"), "values")}, false, usage, err
	case "gemini-batch-embeddings":
		var values []oif.Value
		for _, embedding := range operations.Member(root, "embeddings").Elements() {
			values = append(values, operations.Member(embedding, "values"))
		}
		usage, err := geminiUsage(root)
		return values, false, usage, err
	case "vertex-embeddings":
		var values []oif.Value
		var tokens int64
		observed := true
		for _, prediction := range operations.Member(root, "predictions").Elements() {
			embedding := operations.Member(prediction, "embeddings")
			values = append(values, operations.Member(embedding, "values"))
			n, ok := operations.Int(operations.Member(operations.Member(embedding, "statistics"), "token_count"))
			if !ok || n < 0 {
				observed = false
			} else {
				tokens += n
			}
		}
		var usage *operations.Usage
		if observed {
			usage = &operations.Usage{InputTokens: &tokens, TotalTokens: &tokens}
		}
		return values, false, usage, nil
	case "bedrock-embeddings":
		return extractBedrockResult(source, root, request)
	default:
		if root.Kind() != oif.Array {
			return nil, false, nil, operations.Violation("/", "native_vector_batch")
		}
		return root.Elements(), false, nil, nil
	}
}

func extractBedrockResult(source oif.Request, root oif.Value, request Request) ([]oif.Value, bool, *operations.Usage, error) {
	var values []oif.Value
	if request.format.Layout == "native-typed" {
		byType := operations.Member(root, "embeddingsByType")
		if byType.Kind() != oif.Object {
			return nil, false, nil, operations.Violation("/embeddingsByType", "native_dtype_map")
		}
		requested := operations.Member(source.Document().Root(), "embeddingTypes")
		if len(byType.Members()) != len(requested.Elements()) {
			return nil, false, nil, operations.Violation("/embeddingsByType", "requested_native_dtypes")
		}
		for _, kind := range requested.Elements() {
			if _, present := byType.Lookup(operations.String(kind)); !present {
				return nil, false, nil, operations.Violation("/embeddingsByType", "requested_native_dtypes")
			}
		}
		for _, entry := range byType.Members() {
			format := request.format
			format.Layout, format.DType, format.BitsPerDimension = "dense", "float32", 32
			if entry.Name == "binary" {
				format.Layout, format.DType, format.BitsPerDimension = "binary", "binary", 1
			} else if entry.Name != "float" {
				return nil, false, nil, operations.Violation("/embeddingsByType", "native_dtype_map")
			}
			if _, err := validateVector(entry.Value, format, 0); err != nil {
				return nil, false, nil, err
			}
		}
	} else {
		values = []oif.Value{operations.Member(root, "embedding")}
	}
	var usage *operations.Usage
	if token, present := root.Lookup("inputTextTokenCount"); present {
		n, ok := operations.Int(token)
		if !ok || n < 0 {
			return nil, false, nil, operations.Violation("/inputTextTokenCount", "token_usage")
		}
		usage = &operations.Usage{InputTokens: &n, TotalTokens: &n}
	}
	return values, false, usage, nil
}

func validateVector(value oif.Value, format Format, index int) (Vector, error) {
	vector := Vector{InputIndex: index, Source: value, Format: format}
	if format.Encoding == "base64" {
		if value.Kind() != oif.String {
			return vector, operations.Violation("/embedding", "base64_storage")
		}
		length, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(operations.String(value))))
		if err != nil || length < 1 {
			return vector, operations.Violation("/embedding", "base64_storage")
		}
		vector.StoredBytes = int(length)
		width := 1
		if format.DType == "float32" {
			width = 4
		}
		if vector.StoredBytes%width != 0 {
			return vector, operations.Violation("/embedding", "dtype_byte_width")
		}
		vector.StoredElements = vector.StoredBytes / width
	} else {
		if value.Kind() != oif.Array {
			return vector, operations.Violation("/embedding", "native_vector_storage")
		}
		vector.StoredElements = len(value.Elements())
		if format.Layout == "sparse" {
			for _, entry := range value.Elements() {
				if _, ok := operations.Uint(operations.Member(entry, "index")); !ok || !operations.Number(operations.Member(entry, "value")) {
					return vector, operations.Violation("/embedding", "sparse_coordinate")
				}
			}
			return vector, nil
		}
		if format.Layout == "multivector" {
			vector.TokenVectors = len(value.Elements())
			var width int
			for _, token := range value.Elements() {
				dense := format
				dense.Layout = "dense"
				part, err := validateVector(token, dense, index)
				if err != nil {
					return vector, err
				}
				if width != 0 && width != part.StoredElements {
					return vector, operations.Violation("/embedding", "token_vector_dimensions")
				}
				width = part.StoredElements
			}
			if width > 0 {
				vector.Format.LogicalDimensions = int64(width)
				vector.Format.DimensionsKnown = true
			}
			return vector, nil
		}
		if vector.StoredElements == 0 {
			return vector, operations.Violation("/embedding", "nonempty_vector")
		}
		for _, element := range value.Elements() {
			if format.DType == "float32" || format.DType == "float" {
				if !operations.Number(element) {
					return vector, operations.Violation("/embedding", "native_numeric_value")
				}
				continue
			}
			n, ok := operations.Int(element)
			if !ok || format.DType == "int8" && (n < -128 || n > 127) || format.DType == "uint8" && (n < 0 || n > 255) || format.DType == "binary" && (n != 0 && n != 1) {
				return vector, operations.Violation("/embedding", "native_integer_dtype")
			}
		}
	}
	logical := int64(vector.StoredElements)
	if format.Layout == "packed-binary" {
		logical *= 8
	}
	if format.DimensionsKnown && logical != format.LogicalDimensions {
		return vector, operations.Violation("/embedding", "logical_storage_shape")
	}
	vector.Format.LogicalDimensions, vector.Format.DimensionsKnown = logical, true
	return vector, nil
}

// geminiUsage is the usage a Gemini embedding response reports in its
// usageMetadata: the tokens of the prompt. A response that reports none has no
// usage.
func geminiUsage(root oif.Value) (*operations.Usage, error) {
	metadata := operations.Member(root, "usageMetadata")
	if operations.Optional(metadata) {
		return nil, nil
	}
	if metadata.Kind() != oif.Object {
		return nil, operations.Violation("/usageMetadata", "native_usage")
	}
	token := operations.Member(metadata, "promptTokenCount")
	if operations.Optional(token) {
		return nil, nil
	}
	n, ok := operations.Int(token)
	if !ok || n < 0 {
		return nil, operations.Violation("/usageMetadata/promptTokenCount", "native_usage")
	}
	return &operations.Usage{InputTokens: &n, TotalTokens: &n}, nil
}

func tokenUsage(value oif.Value, totalIsInput bool) (*operations.Usage, error) {
	if value.Kind() != oif.Object {
		return nil, operations.Violation("/usage", "native_usage")
	}
	name := "prompt_tokens"
	if totalIsInput {
		name = "total_tokens"
	}
	n, ok := operations.Int(operations.Member(value, name))
	if !ok || n < 0 {
		return nil, operations.Violation("/usage", "native_usage")
	}
	total, ok := operations.Int(operations.Member(value, "total_tokens"))
	if !ok || total < n {
		return nil, operations.Violation("/usage", "native_usage")
	}
	return &operations.Usage{InputTokens: &n, TotalTokens: &total}, nil
}

func cloneUsage(value *operations.Usage) *operations.Usage {
	if value == nil {
		return nil
	}
	out := *value
	copy := func(p *int64) *int64 {
		if p == nil {
			return nil
		}
		n := *p
		return &n
	}
	out.InputTokens = copy(value.InputTokens)
	out.OutputTokens = copy(value.OutputTokens)
	out.TotalTokens = copy(value.TotalTokens)
	return &out
}

func googleModels(doc oif.Document, model string) ([]oif.Change, error) {
	model = "models/" + strings.TrimPrefix(model, "models/")
	value, _ := json.Marshal(model)
	changes := []oif.Change{}
	if _, present := doc.Root().Lookup("model"); present {
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(value), Origin: oif.IdentityBinding, Reason: "published native model binding"})
	}
	for index, request := range operations.Member(doc.Root(), "requests").Elements() {
		if _, present := request.Lookup("model"); present {
			changes = append(changes, oif.Change{Pointer: "/requests/" + strconv.Itoa(index) + "/model", Value: string(value), Origin: oif.IdentityBinding, Reason: "published native model binding"})
		}
	}
	return changes, nil
}

func probe(id, model string) []byte {
	var body any
	switch id {
	case "openai-embeddings", "voyage-embeddings":
		body = map[string]any{"model": model, "input": "embedding probe"}
	case "cohere-embed-v2":
		body = map[string]any{"model": model, "input_type": "search_document", "texts": []string{"embedding probe"}, "embedding_types": []string{"float"}}
	case "gemini-embeddings":
		body = map[string]any{"content": map[string]any{"parts": []any{map[string]string{"text": "embedding probe"}}}}
	case "gemini-batch-embeddings":
		body = map[string]any{"requests": []any{map[string]any{"model": "models/" + model, "content": map[string]any{"parts": []any{map[string]string{"text": "embedding probe"}}}}}}
	case "vertex-embeddings":
		body = map[string]any{"instances": []any{map[string]string{"content": "embedding probe"}}}
	case "bedrock-embeddings":
		body = map[string]string{"inputText": "embedding probe"}
	default:
		body = map[string]string{"inputs": "embedding probe"}
	}
	return operations.Raw(body)
}

func googleRoute(source oif.Request, route string) error {
	root := source.Document().Root()
	check := func(value oif.Value) error {
		if value.Kind() == oif.Absent {
			return nil
		}
		if value.Kind() != oif.String || strings.TrimPrefix(operations.String(value), "models/") != strings.TrimPrefix(route, "models/") {
			return operations.Error("resource_affinity", "/model", "route_identity", "Nested native model identities must agree with the selected route.")
		}
		return nil
	}
	if err := check(operations.Member(root, "model")); err != nil {
		return err
	}
	for _, entry := range operations.Member(root, "requests").Elements() {
		if err := check(operations.Member(entry, "model")); err != nil {
			return err
		}
	}
	return nil
}

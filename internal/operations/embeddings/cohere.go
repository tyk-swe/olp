package embeddings

import (
	"encoding/base64"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

var cohereInputTypes = []string{"search_document", "search_query", "classification", "clustering", "image"}
var cohereEmbeddingTypes = []string{"float", "int8", "uint8", "binary", "ubinary", "base64"}

func cohereRequestSchema() []byte {
	return operations.Raw(map[string]any{
		"type": "object", "required": []string{"model", "input_type"},
		"anyOf": []any{
			map[string]any{"required": []string{"texts"}},
			map[string]any{"required": []string{"images"}},
			map[string]any{"required": []string{"inputs"}},
		},
		"properties": map[string]any{
			"model":            map[string]any{"type": "string"},
			"input_type":       map[string]any{"type": "string", "enum": cohereInputTypes},
			"texts":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"images":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"inputs":           map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"embedding_types":  map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": cohereEmbeddingTypes}},
			"output_dimension": map[string]any{"type": "integer", "enum": []int{256, 512, 1024, 1536}},
			"truncate":         map[string]any{"type": "string", "enum": []string{"NONE", "START", "END"}},
		},
	})
}

func cohereDefaults() map[string]operations.Field {
	enum := func(name string, values []string) operations.Field {
		return operations.FieldSchema(map[string]any{"type": "string", "enum": values}, func(v oif.Value) error {
			if v.Kind() != oif.String || !slices.Contains(values, operations.String(v)) {
				return operations.Invalid(name, "This native Cohere v2 value is not supported.")
			}
			return nil
		})
	}
	positive := func(name string, values []int) operations.Field {
		schema := map[string]any{"type": "integer", "minimum": 1}
		if values != nil {
			schema["enum"] = values
		}
		return operations.FieldSchema(schema, func(v oif.Value) error {
			n, ok := operations.Int(v)
			if !ok || n < 1 || values != nil && !slices.Contains(values, int(n)) {
				return operations.Invalid(name, "Use a supported positive native Cohere v2 value.")
			}
			return nil
		})
	}
	return map[string]operations.Field{
		"input_type":       enum("input_type", cohereInputTypes),
		"truncate":         enum("truncate", []string{"NONE", "START", "END"}),
		"output_dimension": positive("output_dimension", []int{256, 512, 1024, 1536}),
		"max_tokens":       positive("max_tokens", nil),
		"embedding_types": operations.FieldSchema(map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "enum": cohereEmbeddingTypes}}, func(v oif.Value) error {
			_, err := requestedCohereTypes(v)
			return err
		}),
		"priority": operations.FieldSchema(map[string]any{"type": "integer", "minimum": 0, "maximum": 999}, func(v oif.Value) error {
			n, ok := operations.Int(v)
			if !ok || n < 0 || n > 999 {
				return operations.Invalid("priority", "Use a native Cohere priority from 0 to 999.")
			}
			return nil
		}),
	}
}

func requestedCohereTypes(value oif.Value) ([]string, error) {
	if value.Kind() == oif.Absent {
		return []string{"float"}, nil
	}
	if value.Kind() != oif.Array || len(value.Elements()) == 0 || len(value.Elements()) > len(cohereEmbeddingTypes) {
		return nil, operations.Invalid("embedding_types", "Use a non-empty native Cohere embedding type set.")
	}
	out := make([]string, 0, len(value.Elements()))
	for _, entry := range value.Elements() {
		kind := operations.String(entry)
		if entry.Kind() != oif.String || !slices.Contains(cohereEmbeddingTypes, kind) || slices.Contains(out, kind) {
			return nil, operations.Invalid("embedding_types", "Use distinct native float, integer, binary or base64 types.")
		}
		out = append(out, kind)
	}
	return out, nil
}

func cohereFormat(kind string) Format {
	format := Format{Layout: "dense", DType: "float", Encoding: "array", BitsPerDimension: 32}
	switch kind {
	case "int8", "uint8":
		format.DType, format.BitsPerDimension = kind, 8
	case "binary":
		format.Layout, format.DType, format.BitsPerDimension = "packed-binary", "int8", 1
	case "ubinary":
		format.Layout, format.DType, format.BitsPerDimension = "packed-binary", "uint8", 1
	case "base64":
		format.Layout, format.DType, format.Encoding, format.BitsPerDimension = "native-encoded", "native", "base64", 0
	}
	return format
}

// Cohere documents base64 storage but does not define a byte-to-vector-element
// width for it. Validate the encoding without inventing float32 dimensions.
func cohereBase64Vector(value oif.Value, format Format, index int) (Vector, error) {
	vector := Vector{InputIndex: index, Source: value, Format: format}
	if value.Kind() != oif.String {
		return vector, operations.Violation("/embeddings/base64", "base64_storage")
	}
	length, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(operations.String(value))))
	if err != nil || length < 1 {
		return vector, operations.Violation("/embeddings/base64", "base64_storage")
	}
	vector.StoredBytes = int(length)
	return vector, nil
}

func cohereImage(value oif.Value) (int64, error) {
	if value.Kind() != oif.String {
		return 0, operations.Invalid("images", "Use an original image data URI.")
	}
	prefix, data, found := strings.Cut(operations.String(value), ",")
	if !found || !slices.Contains([]string{"data:image/jpeg;base64", "data:image/png;base64", "data:image/webp;base64", "data:image/gif;base64"}, strings.ToLower(prefix)) {
		return 0, operations.Invalid("images", "Use a supported original image data URI.")
	}
	length, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data)))
	if err != nil || length == 0 || length > 20<<20 {
		return 0, operations.Invalid("images", "The original image data URI is invalid or exceeds the native bound.")
	}
	return length, nil
}

func liftCohereRequest(source oif.Request) (Request, error) {
	r := Request{source: source, dialect: "cohere-embed-v2", format: cohereFormat("float"), estimate: 1}
	root := source.Document().Root()
	if root.Kind() != oif.Object || operations.Member(root, "model").Kind() != oif.String || operations.String(operations.Member(root, "model")) == "" {
		return r, operations.Invalid("model", "Use the native Cohere v2 model identity.")
	}
	inputType := operations.Member(root, "input_type")
	if inputType.Kind() != oif.String || !slices.Contains(cohereInputTypes, operations.String(inputType)) {
		return r, operations.Invalid("input_type", "Use a declared native Cohere embedding task.")
	}
	for _, name := range []string{"input", "output_dtype", "dimensions", "encoding_format", "sparse", "multivector"} {
		if _, present := root.Lookup(name); present {
			return r, operations.Invalid(name, "Use the native Cohere v2 embedding controls instead of a foreign alias.")
		}
	}
	types, err := requestedCohereTypes(operations.Member(root, "embedding_types"))
	if err != nil {
		return r, err
	}
	r.format = cohereFormat(types[0])
	if len(types) > 1 {
		r.format = Format{Layout: "native-typed", DType: "native"}
	}
	if dimension := operations.Member(root, "output_dimension"); dimension.Kind() != oif.Absent {
		n, ok := operations.Int(dimension)
		if !ok || !slices.Contains([]int64{256, 512, 1024, 1536}, n) {
			return r, operations.Invalid("output_dimension", "Use a supported Embed v4 native output dimension.")
		}
		r.format.LogicalDimensions, r.format.DimensionsKnown = n, true
	}
	for _, name := range []string{"max_tokens", "priority", "truncate"} {
		if value, present := root.Lookup(name); present {
			if err := cohereDefaults()[name].Validate(value); err != nil {
				return r, err
			}
		}
	}
	var selected oif.Value
	for _, name := range []string{"texts", "images", "inputs"} {
		if value, present := root.Lookup(name); present {
			if selected.Kind() != oif.Absent {
				return r, operations.Invalid(name, "Choose exactly one native Cohere input collection.")
			}
			selected = value
		}
	}
	if selected.Kind() != oif.Array || len(selected.Elements()) == 0 || len(selected.Elements()) > 96 {
		return r, operations.Invalid("texts", "Use between 1 and 96 native Cohere inputs.")
	}
	r.inputs = len(selected.Elements())
	var imageBytes int64
	for _, item := range selected.Elements() {
		switch {
		case operations.Member(root, "texts").Kind() != oif.Absent:
			if item.Kind() != oif.String {
				return r, operations.Invalid("texts", "Native text inputs must be strings.")
			}
			r.estimate += int64((len(operations.String(item)) + 3) / 4)
		case operations.Member(root, "images").Kind() != oif.Absent:
			n, err := cohereImage(item)
			if err != nil {
				return r, err
			}
			imageBytes += n
			r.estimate += 2000
		default:
			content := operations.Member(item, "content")
			if item.Kind() != oif.Object || content.Kind() != oif.Array || len(content.Elements()) == 0 {
				return r, operations.Invalid("inputs", "Each native multimodal input requires ordered content.")
			}
			for _, part := range content.Elements() {
				switch operations.String(operations.Member(part, "type")) {
				case "text":
					text := operations.Member(part, "text")
					if text.Kind() != oif.String {
						return r, operations.Invalid("inputs", "Native text content requires a string.")
					}
					r.estimate += int64((len(operations.String(text)) + 3) / 4)
				case "image_url":
					n, err := cohereImage(operations.Member(operations.Member(part, "image_url"), "url"))
					if err != nil {
						return r, err
					}
					imageBytes += n
					r.estimate += 2000
				default:
					return r, operations.Invalid("inputs", "Native content must be text or an original image URL part.")
				}
			}
		}
	}
	if imageBytes > 20<<20 {
		return r, operations.Invalid("images", "Native Cohere image inputs exceed the combined 20 MiB bound.")
	}
	return r, nil
}

func liftCohereResult(source oif.Request, result oif.Result) (Result, error) {
	request, err := liftCohereRequest(source)
	out := Result{source: result}
	if err != nil {
		return out, err
	}
	root := result.Source().Root()
	byType := operations.Member(root, "embeddings")
	if root.Kind() != oif.Object || byType.Kind() != oif.Object {
		return out, operations.Violation("/embeddings", "native_dtype_map")
	}
	if echoed, present := root.Lookup("texts"); present && echoed.Kind() != oif.Null {
		if echoed.Kind() != oif.Array {
			return out, operations.Violation("/texts", "native_text_echo")
		}
		if original := operations.Member(source.Document().Root(), "texts"); original.Kind() == oif.Array {
			if len(echoed.Elements()) != len(original.Elements()) {
				return out, operations.Violation("/texts", "input_text_correspondence")
			}
			for index, text := range echoed.Elements() {
				if text.Kind() != oif.String || operations.String(text) != operations.String(original.Elements()[index]) {
					return out, operations.Violation("/texts", "input_text_correspondence")
				}
			}
		}
	}
	types, _ := requestedCohereTypes(operations.Member(source.Document().Root(), "embedding_types"))
	var commonDimensions int64
	for _, kind := range types {
		values := operations.Member(byType, kind)
		if values.Kind() != oif.Array || len(values.Elements()) != request.inputs {
			return out, operations.Violation("/embeddings/"+kind, "input_vector_correspondence")
		}
		format := cohereFormat(kind)
		format.LogicalDimensions, format.DimensionsKnown = request.format.LogicalDimensions, request.format.DimensionsKnown
		for index, value := range values.Elements() {
			var vector Vector
			if kind == "base64" {
				vector, err = cohereBase64Vector(value, format, index)
			} else {
				vector, err = validateVector(value, format, index)
			}
			if err != nil {
				return out, err
			}
			if vector.Format.DimensionsKnown {
				if commonDimensions != 0 && commonDimensions != vector.Format.LogicalDimensions {
					return out, operations.Violation("/embeddings/"+kind, "native_dtype_dimensions")
				}
				commonDimensions = vector.Format.LogicalDimensions
			}
			out.vectors = append(out.vectors, vector)
		}
	}
	if commonDimensions != 0 {
		for index := range out.vectors {
			if !out.vectors[index].Format.DimensionsKnown {
				out.vectors[index].Format.LogicalDimensions = commonDimensions
				out.vectors[index].Format.DimensionsKnown = true
			}
		}
	}
	if billed := operations.Member(operations.Member(root, "meta"), "billed_units"); billed.Kind() == oif.Object {
		if tokens := operations.Member(billed, "input_tokens"); tokens.Kind() != oif.Absent && tokens.Kind() != oif.Null {
			n, ok := operations.Int(tokens)
			if !ok || n < 0 {
				return out, operations.Violation("/meta/billed_units/input_tokens", "native_usage")
			}
			out.usage = &operations.Usage{InputTokens: &n, TotalTokens: &n}
		}
		if images := operations.Member(billed, "images"); images.Kind() != oif.Absent && images.Kind() != oif.Null {
			if !operations.Number(images) || strings.HasPrefix(images.Raw(), "-") {
				return out, operations.Violation("/meta/billed_units/images", "native_usage")
			}
			if out.usage == nil {
				out.usage = &operations.Usage{}
			}
			raw := images.Raw()
			out.usage.MediaUnits = &raw
		}
	}
	return out, nil
}

func cohereInputText(request oif.Request) ([]operations.Text, error) {
	root := request.Document().Root()
	known := map[string]bool{"model": true, "texts": true, "images": true, "inputs": true}
	for field := range cohereDefaults() {
		known[field] = true
	}
	for _, field := range root.Members() {
		if !known[field.Name] {
			return nil, operations.Error("policy_conflict", "/native_extension", "input_policy_coverage", "The text policy cannot inspect an unknown native Cohere embedding control.")
		}
	}
	if operations.Member(root, "images").Kind() != oif.Absent {
		return nil, operations.Error("policy_conflict", "/images", "media_policy_coverage", "The text policy cannot inspect native image bytes.")
	}
	out := []operations.Text{}
	for index, value := range operations.Member(root, "texts").Elements() {
		out = append(out, operations.Text{Pointer: "/texts/" + strconv.Itoa(index), Value: operations.String(value)})
	}
	for i, item := range operations.Member(root, "inputs").Elements() {
		for _, field := range item.Members() {
			if field.Name != "content" {
				return nil, operations.Error("policy_conflict", "/inputs/"+strconv.Itoa(i), "input_policy_coverage", "The text policy cannot inspect an unknown native Cohere input member.")
			}
		}
		for j, part := range operations.Member(item, "content").Elements() {
			if operations.String(operations.Member(part, "type")) == "image_url" {
				return nil, operations.Error("policy_conflict", "/inputs", "media_policy_coverage", "The text policy cannot inspect native image bytes.")
			}
			for _, field := range part.Members() {
				if field.Name != "type" && field.Name != "text" {
					return nil, operations.Error("policy_conflict", "/inputs/"+strconv.Itoa(i)+"/content/"+strconv.Itoa(j), "input_policy_coverage", "The text policy cannot inspect an unknown native Cohere text part member.")
				}
			}
			out = append(out, operations.Text{Pointer: "/inputs/" + strconv.Itoa(i) + "/content/" + strconv.Itoa(j) + "/text", Value: operations.String(operations.Member(part, "text"))})
		}
	}
	return out, nil
}

// Package embeddings retains native vector storage and its logical shape.
// Values remain source spans; validation never casts vectors or normalizes them.
package embeddings

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

var identity = oif.Identity{ID: "embeddings", Revision: operations.Revision}

func Identity() oif.Identity { return identity }

type Format struct {
	Layout, DType, Encoding string
	LogicalDimensions       int64
	DimensionsKnown         bool
	BitsPerDimension        int
}

type Request struct {
	source   oif.Request
	dialect  string
	inputs   int
	format   Format
	estimate int64
}

func (r Request) Schema() oif.Identity { return identity }
func (r Request) Source() oif.Request  { return r.source }
func (r Request) Inputs() int          { return r.inputs }
func (r Request) Format() Format       { return r.format }

// Vector identifies one input's native storage. Sparse coordinates and nested
// token vectors remain in Source; LogicalDimensions is unknown where native API
// omitted it. StoredElements never masquerades as packed logical dimensions.
type Vector struct {
	InputIndex                                int
	Source                                    oif.Value
	Format                                    Format
	StoredElements, StoredBytes, TokenVectors int
}
type Result struct {
	source  oif.Result
	vectors []Vector
	usage   *operations.Usage
}

func (r Result) Schema() oif.Identity { return identity }
func (r Result) Source() oif.Result   { return r.source }
func (r Result) Vectors() []Vector    { return slices.Clone(r.vectors) }

func Definitions() []operations.Dialect {
	definitions := []struct{ id, surface, path, relative string }{
		{"openai-embeddings", "openai", "embeddings", ""},
		{"voyage-embeddings", "native", "embeddings", ""},
		{"gemini-embeddings", "gemini", "gemini_embeddings", ""},
		{"gemini-batch-embeddings", "gemini", "gemini_embeddings_batch", ""},
		{"vertex-embeddings", "native", "vertex_embeddings", ""},
		{"bedrock-embeddings", "bedrock", "bedrock_embeddings", ""},
		{"cohere-embed-v2", "native", "", "embed"},
		{"tei-embeddings", "native", "", "embed"},
		{"tei-sparse-embeddings", "native", "", "embed_sparse"},
		{"tei-multivector-embeddings", "native", "", "embed_all"},
	}
	out := make([]operations.Dialect, 0, len(definitions))
	for _, definition := range definitions {
		id := definition.id
		d := operations.Dialect{Identity: oif.Identity{ID: id, Revision: operations.Revision}, Operation: identity, Surface: definition.surface, Label: id, Address: operations.Address{LegacyPath: definition.path, RelativePath: definition.relative}, Evidence: "native-embedding-storage/1"}
		d.Request = func(source oif.Request) (oif.View, error) {
			if id == "cohere-embed-v2" {
				return liftCohereRequest(source)
			}
			return liftRequest(source, id)
		}
		d.Result = func(request oif.Request, result oif.Result) (oif.View, error) {
			if id == "cohere-embed-v2" {
				return liftCohereResult(request, result)
			}
			return liftResult(request, result, id)
		}
		d.Estimate = func(view oif.View) int64 { return view.(Request).estimate }
		d.Usage = func(view oif.View) *operations.Usage { return cloneUsage(view.(Result).usage) }
		d.RequiredClient = func(view oif.View) string {
			r := view.(Request)
			if r.format.DType != "float32" && r.format.DType != "float" || r.format.Layout != "dense" {
				return operations.RawVectorClient
			}
			return ""
		}
		d.Probe = func(model string) []byte { return probe(id, model) }
		d.InputText = func(request oif.Request) ([]operations.Text, error) {
			if id == "cohere-embed-v2" {
				return cohereInputText(request)
			}
			return inputText(request, id)
		}
		if id == "openai-embeddings" || id == "voyage-embeddings" || id == "cohere-embed-v2" {
			d.BindModel = operations.ModelChanges
			d.ModelBinding = operations.ModelRequired
			if id != "cohere-embed-v2" {
				d.BindResultModel = operations.ModelChanges
			}
		}
		if id == "gemini-embeddings" || id == "gemini-batch-embeddings" {
			d.BindModel = googleModels
			d.ModelBinding = operations.ModelOptional
			d.ValidateRoute = googleRoute
		}
		d.Defaults = defaults(id)
		d.RequestSchema = operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}}, inputField(id))
		if id == "cohere-embed-v2" {
			d.Defaults = cohereDefaults()
			d.RequestSchema = cohereRequestSchema()
		}
		d.ResultSchema = operations.Raw(map[string]any{"description": "Native vector records and representation, without conversion."})
		d.Documentation = "https://developers.openai.com/api/reference/resources/embeddings/methods/create"
		if id == "voyage-embeddings" {
			d.Documentation = "https://docs.voyageai.com/reference/embeddings-api"
		}
		if id == "cohere-embed-v2" {
			d.Documentation = "https://docs.cohere.com/reference/embed"
		}
		if strings.HasPrefix(id, "tei-") {
			d.Documentation = "https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/types.rs"
		}
		out = append(out, d)
	}
	return out
}

func inputField(id string) string {
	switch id {
	case "openai-embeddings", "voyage-embeddings":
		return "input"
	case "gemini-embeddings":
		return "content"
	case "gemini-batch-embeddings":
		return "requests"
	case "vertex-embeddings":
		return "instances"
	case "bedrock-embeddings":
		return "inputText"
	default:
		return "inputs"
	}
}

func liftRequest(source oif.Request, id string) (Request, error) {
	r := Request{source: source, dialect: id, format: Format{Layout: "dense", DType: "float", Encoding: "array", BitsPerDimension: 0}, estimate: 1}
	if id == "voyage-embeddings" || id == "bedrock-embeddings" || strings.HasPrefix(id, "tei-") {
		r.format.DType = "float32"
		r.format.BitsPerDimension = 32
	}
	root := source.Document().Root()
	if root.Kind() != oif.Object {
		return r, operations.Invalid("request", "The native embedding request must be an object.")
	}
	var texts []operations.Text
	var err error
	input := operations.Member(root, inputField(id))
	switch id {
	case "openai-embeddings", "voyage-embeddings":
		if operations.Member(root, "model").Kind() != oif.String {
			return r, operations.Invalid("model", "The embedding model must be a string.")
		}
		r.inputs, texts, err = operations.Strings(input, id == "openai-embeddings")
		if id == "voyage-embeddings" && r.inputs > 1000 {
			return r, operations.Invalid("input", "Voyage accepts at most 1000 input texts.")
		}
		if id == "voyage-embeddings" {
			if dtype, present := root.Lookup("output_dtype"); present {
				switch operations.String(dtype) {
				case "float":
				case "int8", "uint8":
					r.format.DType = operations.String(dtype)
					r.format.BitsPerDimension = 8
				case "binary":
					r.format.DType = "int8"
					r.format.Layout = "packed-binary"
					r.format.BitsPerDimension = 1
				case "ubinary":
					r.format.DType = "uint8"
					r.format.Layout = "packed-binary"
					r.format.BitsPerDimension = 1
				default:
					return r, operations.Invalid("output_dtype", "The native vector dtype is not supported by this dialect.")
				}
			}
			if v, present := root.Lookup("input_type"); present && !operations.Optional(v) && !slices.Contains([]string{"query", "document"}, operations.String(v)) {
				return r, operations.Invalid("input_type", "Use query, document or native null.")
			}
			if v, present := root.Lookup("truncation"); present && v.Kind() != oif.Boolean {
				return r, operations.Invalid("truncation", "Use a native boolean truncation setting.")
			}
		} else {
			for _, name := range []string{"output_dtype", "output_dimension", "input_type", "truncation", "normalize"} {
				if _, present := root.Lookup(name); present {
					return r, operations.Error("target_capability", "/"+name, "native_parameter_owner", "This parameter requires its registered native embedding dialect.")
				}
			}
		}
		if encoding, present := root.Lookup("encoding_format"); present {
			value := operations.String(encoding)
			if value == "base64" {
				r.format.Encoding = "base64"
				if id == "openai-embeddings" {
					r.format.DType = "float32"
					r.format.BitsPerDimension = 32
				}
			} else if !(id == "voyage-embeddings" && encoding.Kind() == oif.Null || id == "openai-embeddings" && value == "float") {
				return r, operations.Invalid("encoding_format", "This encoding format has no native representation in the selected dialect.")
			}
		}
	case "gemini-embeddings":
		r.inputs = 1
		texts, err = googleText(input)
	case "gemini-batch-embeddings":
		for _, name := range []string{"outputDimensionality", "taskType", "title"} {
			if _, present := root.Lookup(name); present {
				return r, operations.Invalid(name, "Batch controls belong to each native request member.")
			}
		}
		if input.Kind() != oif.Array || len(input.Elements()) < 1 || len(input.Elements()) > 100 {
			return r, operations.Invalid("requests", "Use between 1 and 100 native embedding requests.")
		}
		r.inputs = len(input.Elements())
		for _, entry := range input.Elements() {
			if dim := operations.Member(entry, "outputDimensionality"); !operations.Optional(dim) {
				n, ok := operations.Int(dim)
				if !ok || n <= 0 || n > 1<<20 {
					return r, operations.Invalid("requests/outputDimensionality", "Use a bounded positive member dimension.")
				}
			}
			part, e := googleText(operations.Member(entry, "content"))
			if e != nil {
				return r, e
			}
			texts = append(texts, part...)
		}
	case "vertex-embeddings":
		if input.Kind() != oif.Array || len(input.Elements()) == 0 {
			return r, operations.Invalid("instances", "Use native embedding instances.")
		}
		r.inputs = len(input.Elements())
		for _, entry := range input.Elements() {
			v := operations.Member(entry, "content")
			if v.Kind() != oif.String {
				return r, operations.Invalid("instances", "Each native instance requires content text.")
			}
			texts = append(texts, operations.Text{Value: operations.String(v)})
		}
	case "bedrock-embeddings":
		if input.Kind() != oif.String {
			return r, operations.Invalid("inputText", "The Titan text input must be a string.")
		}
		r.inputs = 1
		texts = []operations.Text{{Value: operations.String(input)}}
		if types, present := root.Lookup("embeddingTypes"); present {
			if types.Kind() != oif.Array || len(types.Elements()) == 0 {
				return r, operations.Invalid("embeddingTypes", "Use native float or binary embedding types.")
			}
			for _, v := range types.Elements() {
				if operations.String(v) != "float" && operations.String(v) != "binary" {
					return r, operations.Invalid("embeddingTypes", "The requested Titan representation is unavailable.")
				}
			}
			if len(types.Elements()) != 1 || operations.String(types.Elements()[0]) != "float" {
				r.format.Layout = "native-typed"
				r.format.DType = "native"
				r.format.BitsPerDimension = 0
			}
		}
	default:
		r.inputs, texts, err = operations.Strings(input, true)
		if id != "tei-embeddings" {
			if _, present := root.Lookup("dimensions"); present {
				return r, operations.Invalid("dimensions", "This native operation has no dimension control.")
			}
		}
		if id == "tei-sparse-embeddings" {
			r.format.Layout = "sparse"
		}
		if id == "tei-multivector-embeddings" {
			r.format.Layout = "multivector"
		}
		if v, present := root.Lookup("normalize"); present && id != "tei-embeddings" {
			return r, operations.Invalid("normalize", "This native embedding operation has no normalization control.")
		} else if present && v.Kind() != oif.Boolean {
			return r, operations.Invalid("normalize", "Use a native boolean normalization control.")
		}
		if v, present := root.Lookup("truncate"); present && !operations.Optional(v) && v.Kind() != oif.Boolean {
			return r, operations.Invalid("truncate", "Use a boolean or native null.")
		}
		if v, present := root.Lookup("prompt_name"); present && !operations.Optional(v) && v.Kind() != oif.String {
			return r, operations.Invalid("prompt_name", "Use a prompt name or native null.")
		}
		if v, present := root.Lookup("truncation_direction"); present && !slices.Contains([]string{"left", "Left", "right", "Right"}, operations.String(v)) {
			return r, operations.Invalid("truncation_direction", "Use the native left or right direction.")
		}
	}
	if err != nil {
		return r, err
	}
	for _, text := range texts {
		r.estimate += int64((len(text.Value) + 3) / 4)
	}
	dimension := operations.Member(root, "dimensions")
	switch id {
	case "voyage-embeddings":
		dimension = operations.Member(root, "output_dimension")
	case "gemini-embeddings":
		dimension = operations.Member(root, "outputDimensionality")
	case "vertex-embeddings":
		dimension = operations.Member(operations.Member(root, "parameters"), "outputDimensionality")
	}
	if !operations.Optional(dimension) {
		count, ok := operations.Int(dimension)
		if !ok || count < 1 || count > 1<<20 {
			return r, operations.Invalid("dimensions", "Use a positive bounded native dimension.")
		}
		if r.format.Layout == "packed-binary" && count%8 != 0 {
			return r, operations.Invalid("output_dimension", "Packed binary logical dimensions must be divisible by eight.")
		}
		r.format.LogicalDimensions, r.format.DimensionsKnown = count, true
	}
	return r, nil
}

func googleText(content oif.Value) ([]operations.Text, error) {
	parts := operations.Member(content, "parts")
	if parts.Kind() != oif.Array || len(parts.Elements()) == 0 {
		return nil, operations.Invalid("content", "Native embedding content requires parts.")
	}
	out := []operations.Text{}
	for _, part := range parts.Elements() {
		if _, present := part.Lookup("fileData"); present {
			return nil, operations.Error("resource_affinity", "/content/parts/fileData", "owned_resource", "Provider resource references need an admitted ownership binding.")
		}
		if _, present := part.Lookup("inlineData"); present {
			return nil, operations.Error("target_capability", "/content/parts/inlineData", "embedding_media_contract", "This embedding dialect has not qualified inline media limits.")
		}
		if text, present := part.Lookup("text"); present {
			if text.Kind() != oif.String {
				return nil, operations.Invalid("content", "Text parts require native strings.")
			}
			out = append(out, operations.Text{Value: operations.String(text)})
		}
	}
	return out, nil
}

func liftResult(source oif.Request, result oif.Result, id string) (Result, error) {
	request, err := liftRequest(source, id)
	out := Result{source: result}
	if err != nil {
		return out, err
	}
	root := result.Source().Root()
	var values []oif.Value
	var indexed bool
	switch id {
	case "openai-embeddings", "voyage-embeddings":
		data := operations.Member(root, "data")
		if data.Kind() != oif.Array {
			return out, operations.Violation("/data", "embedding_collection")
		}
		values = data.Elements()
		indexed = true
		if usage, present := root.Lookup("usage"); present {
			out.usage, err = tokenUsage(usage, id == "voyage-embeddings")
			if err != nil {
				return out, err
			}
		}
	case "gemini-embeddings":
		values = []oif.Value{operations.Member(operations.Member(root, "embedding"), "values")}
	case "gemini-batch-embeddings":
		for _, embedding := range operations.Member(root, "embeddings").Elements() {
			values = append(values, operations.Member(embedding, "values"))
		}
	case "vertex-embeddings":
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
		if observed {
			out.usage = &operations.Usage{InputTokens: &tokens, TotalTokens: &tokens}
		}
	case "bedrock-embeddings":
		if request.format.Layout == "native-typed" {
			byType := operations.Member(root, "embeddingsByType")
			if byType.Kind() != oif.Object {
				return out, operations.Violation("/embeddingsByType", "native_dtype_map")
			}
			requested := operations.Member(source.Document().Root(), "embeddingTypes")
			if len(byType.Members()) != len(requested.Elements()) {
				return out, operations.Violation("/embeddingsByType", "requested_native_dtypes")
			}
			for _, kind := range requested.Elements() {
				if _, present := byType.Lookup(operations.String(kind)); !present {
					return out, operations.Violation("/embeddingsByType", "requested_native_dtypes")
				}
			}
			for _, entry := range byType.Members() {
				format := request.format
				format.Layout = "dense"
				format.DType = "float32"
				format.BitsPerDimension = 32
				if entry.Name == "binary" {
					format.Layout = "binary"
					format.DType = "binary"
					format.BitsPerDimension = 1
				} else if entry.Name != "float" {
					return out, operations.Violation("/embeddingsByType", "native_dtype_map")
				}
				vector, err := validateVector(entry.Value, format, 0)
				if err != nil {
					return out, err
				}
				out.vectors = append(out.vectors, vector)
			}
		} else {
			values = []oif.Value{operations.Member(root, "embedding")}
		}
		if token, present := root.Lookup("inputTextTokenCount"); present {
			n, ok := operations.Int(token)
			if !ok || n < 0 {
				return out, operations.Violation("/inputTextTokenCount", "token_usage")
			}
			out.usage = &operations.Usage{InputTokens: &n, TotalTokens: &n}
		}
	default:
		if root.Kind() != oif.Array {
			return out, operations.Violation("/", "native_vector_batch")
		}
		values = root.Elements()
	}
	if request.format.Layout != "native-typed" && len(values) != request.inputs {
		return out, operations.Violation("/", "input_vector_correspondence")
	}
	seen := map[int]bool{}
	for position, value := range values {
		index := position
		if indexed {
			n, ok := operations.Int(operations.Member(value, "index"))
			if !ok || n < 0 || n >= int64(request.inputs) || seen[int(n)] {
				return out, operations.Violation("/data/index", "input_identity")
			}
			index = int(n)
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
		vector, err := validateVector(value, format, index)
		if err != nil {
			return out, err
		}
		out.vectors = append(out.vectors, vector)
	}
	return out, nil
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

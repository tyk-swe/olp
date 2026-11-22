// Package embeddings retains native vector storage and its logical shape.
// Values remain source spans; validation never casts vectors or normalizes them.
package embeddings

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
)

var identity = oif.Identity{ID: "embeddings", Revision: operations.Revision}

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
	source oif.Result
	usage  *operations.Usage
}

func (r Result) Schema() oif.Identity { return identity }
func (r Result) Source() oif.Result   { return r.source }

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
		d := operations.Dialect{Identity: oif.Identity{ID: id, Revision: operations.Revision}, Operation: identity, Surface: definition.surface, Label: id, Address: operations.Address{FamilyPath: definition.path, RelativePath: definition.relative}, Evidence: "native-embedding-storage/1"}
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
			if id != "cohere-embed-v2" {
				d.BindResultModel = operations.ModelChanges
			}
		}
		if id == "gemini-embeddings" || id == "gemini-batch-embeddings" {
			d.BindModel = googleModels
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

// tokenIDs counts pre-tokenized inputs, reserving one token per ID.
func tokenIDs(input oif.Value) int64 {
	if input.Kind() != oif.Array {
		return 0
	}
	var n int64
	for _, v := range input.Elements() {
		switch v.Kind() {
		case oif.Number:
			n++
		case oif.Array:
			n += int64(len(v.Elements()))
		}
	}
	return n
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
	input := operations.Member(root, inputField(id))
	var texts []operations.Text
	var err error
	switch id {
	case "openai-embeddings", "voyage-embeddings":
		texts, err = liftOpenAIOrVoyageRequest(&r, root, input, id)
	case "gemini-embeddings":
		r.inputs = 1
		texts, err = googleText(input)
	case "gemini-batch-embeddings":
		texts, err = liftGeminiBatchRequest(&r, root, input)
	case "vertex-embeddings":
		texts, err = liftVertexRequest(&r, input)
	case "bedrock-embeddings":
		texts, err = liftBedrockRequest(&r, root, input)
	default:
		texts, err = liftTEIRequest(&r, root, input, id)
	}
	if err != nil {
		return r, err
	}
	for _, text := range texts {
		r.estimate += estimate.HeuristicBytesTokens(len(text.Value))
	}
	if id == "openai-embeddings" || strings.HasPrefix(id, "tei-") {
		r.estimate += tokenIDs(input)
	}
	if err := applyRequestDimension(&r, root, id); err != nil {
		return r, err
	}
	return r, nil
}

func liftOpenAIOrVoyageRequest(r *Request, root, input oif.Value, id string) ([]operations.Text, error) {
	if operations.Member(root, "model").Kind() != oif.String {
		return nil, operations.Invalid("model", "The embedding model must be a string.")
	}
	inputs, texts, err := operations.Strings(input, id == "openai-embeddings")
	r.inputs = inputs
	if id == "voyage-embeddings" && r.inputs > 1000 {
		return nil, operations.Invalid("input", "Voyage accepts at most 1000 input texts.")
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
				return nil, operations.Invalid("output_dtype", "The native vector dtype is not supported by this dialect.")
			}
		}
		if v, present := root.Lookup("input_type"); present && !operations.Optional(v) && !slices.Contains(voyageInputTypes(), operations.String(v)) {
			return nil, operations.Invalid("input_type", "Use query, document or native null.")
		}
		if v, present := root.Lookup("truncation"); present && v.Kind() != oif.Boolean {
			return nil, operations.Invalid("truncation", "Use a native boolean truncation setting.")
		}
	} else {
		for _, name := range []string{"output_dtype", "output_dimension", "input_type", "truncation", "normalize"} {
			if _, present := root.Lookup(name); present {
				return nil, operations.Error("target_capability", "/"+name, "native_parameter_owner", "This parameter requires its registered native embedding dialect.")
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
			return nil, operations.Invalid("encoding_format", "This encoding format has no native representation in the selected dialect.")
		}
	}
	return texts, err
}

func liftGeminiBatchRequest(r *Request, root, input oif.Value) ([]operations.Text, error) {
	for _, name := range []string{"outputDimensionality", "taskType", "title"} {
		if _, present := root.Lookup(name); present {
			return nil, operations.Invalid(name, "Batch controls belong to each native request member.")
		}
	}
	if input.Kind() != oif.Array || len(input.Elements()) < 1 || len(input.Elements()) > 100 {
		return nil, operations.Invalid("requests", "Use between 1 and 100 native embedding requests.")
	}
	r.inputs = len(input.Elements())
	var texts []operations.Text
	for _, entry := range input.Elements() {
		if dim := operations.Member(entry, "outputDimensionality"); !operations.Optional(dim) {
			n, ok := operations.Int(dim)
			if !ok || n <= 0 || n > 1<<20 {
				return nil, operations.Invalid("requests/outputDimensionality", "Use a bounded positive member dimension.")
			}
		}
		part, err := googleText(operations.Member(entry, "content"))
		if err != nil {
			return nil, err
		}
		texts = append(texts, part...)
	}
	return texts, nil
}

func liftVertexRequest(r *Request, input oif.Value) ([]operations.Text, error) {
	if input.Kind() != oif.Array || len(input.Elements()) == 0 {
		return nil, operations.Invalid("instances", "Use native embedding instances.")
	}
	r.inputs = len(input.Elements())
	texts := make([]operations.Text, 0, r.inputs)
	for _, entry := range input.Elements() {
		v := operations.Member(entry, "content")
		if v.Kind() != oif.String {
			return nil, operations.Invalid("instances", "Each native instance requires content text.")
		}
		texts = append(texts, operations.Text{Value: operations.String(v)})
	}
	return texts, nil
}

func liftBedrockRequest(r *Request, root, input oif.Value) ([]operations.Text, error) {
	if input.Kind() != oif.String {
		return nil, operations.Invalid("inputText", "The Titan text input must be a string.")
	}
	r.inputs = 1
	texts := []operations.Text{{Value: operations.String(input)}}
	if types, present := root.Lookup("embeddingTypes"); present {
		if types.Kind() != oif.Array || len(types.Elements()) == 0 {
			return nil, operations.Invalid("embeddingTypes", "Use native float or binary embedding types.")
		}
		for _, v := range types.Elements() {
			if !slices.Contains(bedrockEmbeddingTypes(), operations.String(v)) {
				return nil, operations.Invalid("embeddingTypes", "The requested Titan representation is unavailable.")
			}
		}
		if len(types.Elements()) != 1 || operations.String(types.Elements()[0]) != "float" {
			r.format.Layout = "native-typed"
			r.format.DType = "native"
			r.format.BitsPerDimension = 0
		}
	}
	return texts, nil
}

func liftTEIRequest(r *Request, root, input oif.Value, id string) ([]operations.Text, error) {
	inputs, texts, err := operations.Strings(input, true)
	r.inputs = inputs
	if id != "tei-embeddings" {
		if _, present := root.Lookup("dimensions"); present {
			return nil, operations.Invalid("dimensions", "This native operation has no dimension control.")
		}
	}
	if id == "tei-sparse-embeddings" {
		r.format.Layout = "sparse"
	}
	if id == "tei-multivector-embeddings" {
		r.format.Layout = "multivector"
	}
	if v, present := root.Lookup("normalize"); present && id != "tei-embeddings" {
		return nil, operations.Invalid("normalize", "This native embedding operation has no normalization control.")
	} else if present && v.Kind() != oif.Boolean {
		return nil, operations.Invalid("normalize", "Use a native boolean normalization control.")
	}
	if v, present := root.Lookup("truncate"); present && !operations.Optional(v) && v.Kind() != oif.Boolean {
		return nil, operations.Invalid("truncate", "Use a boolean or native null.")
	}
	if v, present := root.Lookup("prompt_name"); present && !operations.Optional(v) && v.Kind() != oif.String {
		return nil, operations.Invalid("prompt_name", "Use a prompt name or native null.")
	}
	if v, present := root.Lookup("truncation_direction"); present && !slices.Contains(teiTruncationDirections(), operations.String(v)) {
		return nil, operations.Invalid("truncation_direction", "Use the native left or right direction.")
	}
	return texts, err
}

func applyRequestDimension(r *Request, root oif.Value, id string) error {
	dimension := operations.Member(root, "dimensions")
	switch id {
	case "voyage-embeddings":
		dimension = operations.Member(root, "output_dimension")
	case "gemini-embeddings":
		dimension = operations.Member(root, "outputDimensionality")
	case "vertex-embeddings":
		dimension = operations.Member(operations.Member(root, "parameters"), "outputDimensionality")
	}
	if operations.Optional(dimension) {
		return nil
	}
	count, ok := operations.Int(dimension)
	if !ok || count < 1 || count > 1<<20 {
		return operations.Invalid("dimensions", "Use a positive bounded native dimension.")
	}
	if r.format.Layout == "packed-binary" && count%8 != 0 {
		return operations.Invalid("output_dimension", "Packed binary logical dimensions must be divisible by eight.")
	}
	r.format.LogicalDimensions, r.format.DimensionsKnown = count, true
	return nil
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

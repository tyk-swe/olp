package embeddings_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/embeddings"
)

func TestPublishedTEIEmbeddingDirectionsAreAccepted(t *testing.T) {
	for _, id := range []string{"tei-embeddings", "tei-sparse-embeddings", "tei-multivector-embeddings"} {
		d := codec(t, id)
		var schema struct {
			Enum []string `json:"enum"`
		}
		if err := json.Unmarshal(d.Defaults["truncation_direction"].Schema, &schema); err != nil || len(schema.Enum) == 0 {
			t.Fatalf("%s has no truncation directions: %v", id, err)
		}
		for _, direction := range schema.Enum {
			body, err := json.Marshal(map[string]string{"inputs": "text", "truncation_direction": direction})
			if err != nil {
				t.Fatal(err)
			}
			request(t, d, string(body))
		}
	}
}

func TestPublishedBedrockEmbeddingTypesAreAccepted(t *testing.T) {
	d := codec(t, "bedrock-embeddings")
	var schema struct {
		Items struct {
			Enum []string `json:"enum"`
		} `json:"items"`
	}
	if err := json.Unmarshal(d.Defaults["embeddingTypes"].Schema, &schema); err != nil || len(schema.Items.Enum) == 0 {
		t.Fatalf("missing embedding types: %v", err)
	}
	for _, embeddingType := range schema.Items.Enum {
		body, err := json.Marshal(map[string]any{"inputText": "text", "embeddingTypes": []string{embeddingType}})
		if err != nil {
			t.Fatal(err)
		}
		r := request(t, d, string(body))
		value, _ := r.Document().Root().Lookup("embeddingTypes")
		if err := d.Defaults["embeddingTypes"].Validate(value); err != nil {
			t.Errorf("published embedding type %q was rejected as a default: %v", embeddingType, err)
		}
	}
}

func TestPublishedVoyageInputTypesAreAccepted(t *testing.T) {
	d := codec(t, "voyage-embeddings")
	var schema struct {
		Enum []*string `json:"enum"`
	}
	if err := json.Unmarshal(d.Defaults["input_type"].Schema, &schema); err != nil || len(schema.Enum) == 0 {
		t.Fatalf("missing input types: %v", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(d.Defaults["input_type"].Schema))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://contract.test/input-type", document); err != nil {
		t.Fatal(err)
	}
	validator, err := compiler.Compile("https://contract.test/input-type")
	if err != nil {
		t.Fatal(err)
	}
	validate := func(inputType *string) (error, error, error) {
		schemaErr := func() error {
			if inputType == nil {
				return validator.Validate(nil)
			}
			return validator.Validate(*inputType)
		}()
		body, err := json.Marshal(map[string]any{"model": "model", "input": "text", "input_type": inputType})
		if err != nil {
			t.Fatal(err)
		}
		document, err := oif.ParseJSON(body, oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		r, err := oif.NewRequest(oif.Descriptor{Operation: d.Operation, Dialect: d.Identity}, document)
		if err != nil {
			t.Fatal(err)
		}
		_, requestErr := d.Request(r)
		value, _ := document.Root().Lookup("input_type")
		return schemaErr, requestErr, d.Defaults["input_type"].Validate(value)
	}
	for _, inputType := range schema.Enum {
		if schemaErr, requestErr, defaultErr := validate(inputType); schemaErr != nil || requestErr != nil || defaultErr != nil {
			t.Errorf("published input type %v was rejected: schema=%v, request=%v, default=%v", inputType, schemaErr, requestErr, defaultErr)
		}
	}
	unknown := "an unlisted input type"
	for _, inputType := range []*string{nil, &unknown} {
		schemaErr, requestErr, defaultErr := validate(inputType)
		if (schemaErr == nil) != (requestErr == nil) || (schemaErr == nil) != (defaultErr == nil) {
			t.Fatalf("input type %v disagrees with schema: schema=%v, request=%v, default=%v", inputType, schemaErr, requestErr, defaultErr)
		}
	}
}

func codec(t *testing.T, id string) operations.Dialect {
	t.Helper()
	for _, d := range embeddings.Definitions() {
		if d.Identity.ID == id {
			return d
		}
	}
	t.Fatal(id)
	return operations.Dialect{}
}
func request(t *testing.T, d operations.Dialect, raw string) oif.Request {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: d.Operation, Dialect: d.Identity}, doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Request(r); err != nil {
		t.Fatal(err)
	}
	return r
}
func result(t *testing.T, d operations.Dialect, r oif.Request, raw string) (oif.View, error) {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	native, _ := oif.NewResult(r.Descriptor(), doc, oif.Complete)
	return d.Result(r, native)
}

func TestGeminiEmbeddingUsageIsThePromptTokens(t *testing.T) {
	for _, test := range []struct{ id, request, vectors string }{
		{"gemini-embeddings", `{"model":"models/m","content":{"parts":[{"text":"a"}]}}`, `"embedding":{"values":[0.5]}`},
		{"gemini-batch-embeddings", `{"requests":[{"model":"models/m","content":{"parts":[{"text":"a"}]}}]}`, `"embeddings":[{"values":[0.5]}]`},
	} {
		t.Run(test.id, func(t *testing.T) {
			d := codec(t, test.id)
			r := request(t, d, test.request)
			for _, usage := range []struct {
				metadata string
				tokens   *int64
			}{
				{`,"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3}`, ptr(3)},
				{`,"usageMetadata":{"promptTokenCount":3}`, ptr(3)},
				{`,"usageMetadata":{"promptTokenCount":0}`, ptr(0)},
				{``, nil},
				{`,"usageMetadata":null`, nil},
				{`,"usageMetadata":{}`, nil},
				{`,"usageMetadata":{"promptTokenCount":null}`, nil},
			} {
				view, err := result(t, d, r, `{`+test.vectors+usage.metadata+`}`)
				if err != nil {
					t.Fatalf("%s: %v", usage.metadata, err)
				}
				got := d.Usage(view)
				switch {
				case usage.tokens == nil && got != nil:
					t.Fatalf("%s: usage invented: %+v", usage.metadata, got)
				case usage.tokens != nil && (got == nil || *got.InputTokens != *usage.tokens || *got.TotalTokens != *usage.tokens):
					t.Fatalf("%s: usage %+v, want %d prompt tokens", usage.metadata, got, *usage.tokens)
				}
			}
			for _, metadata := range []string{`"x"`, `[3]`, `{"promptTokenCount":-1}`, `{"promptTokenCount":"3"}`, `{"promptTokenCount":1.5}`} {
				if _, err := result(t, d, r, `{`+test.vectors+`,"usageMetadata":`+metadata+`}`); err == nil {
					t.Fatalf("invalid usage accepted: %s", metadata)
				}
			}
		})
	}
}

func ptr(n int64) *int64 { return &n }

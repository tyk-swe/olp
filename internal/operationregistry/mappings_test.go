package operationregistry

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func testRequest(t *testing.T, dialect, operation, raw string) oif.Request {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 1 << 20, MaxNodes: 1 << 12, MaxDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	request, err := oif.NewRequest(oif.Descriptor{Operation: oif.Identity{ID: operation, Revision: "1"}, Dialect: Identity(dialect)}, doc)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func testResult(t *testing.T, operation, raw string) oif.Result {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 1 << 20, MaxNodes: 1 << 12, MaxDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	result, err := oif.NewResult(oif.Descriptor{Operation: oif.Identity{ID: operation, Revision: "1"}, Dialect: Identity("voyage-embeddings")}, doc, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The destination document and its declared changes are co-constructed:
// re-applying the declaration must reproduce the destination byte-for-byte.
func replay(t *testing.T, source, destination oif.Document, declared []oif.Change) {
	t.Helper()
	rebuilt, err := oif.Apply(source, declared)
	if err != nil {
		t.Fatalf("declared construction does not apply: %v", err)
	}
	if string(rebuilt.Bytes()) != string(destination.Bytes()) {
		t.Fatalf("declared changes %v do not reproduce destination %s", declared, destination.Bytes())
	}
}

func byPointer(changes []oif.Change) map[string]oif.Change {
	out := map[string]oif.Change{}
	for _, c := range changes {
		out[c.Pointer] = c
	}
	return out
}

func TestLowerEmbeddingDeclaresConstruction(t *testing.T) {
	source := testRequest(t, "openai-embeddings", "embeddings", `{"model":"route","input":["a","b"],"dimensions":2,"encoding_format":"float"}`)
	doc, declared, err := lowerEmbedding(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay(t, source.Document(), doc, declared)
	set := byPointer(declared)
	if len(set) != 4 {
		t.Fatalf("unexpected declaration: %+v", declared)
	}
	if c := set["/truncation"]; c.Value != "false" || c.Origin != oif.QualifiedMapping || c.Reason == "" {
		t.Fatalf("truncation introduction lacks accounting: %+v", c)
	}
	removal, introduction := set["/dimensions"], set["/output_dimension"]
	if !removal.Remove || introduction.Remove || introduction.Value != "2" || removal.Reason == "" || removal.Reason != introduction.Reason {
		t.Fatalf("dimension relocation lacks paired accounting: %+v", declared)
	}
	if c := set["/encoding_format"]; c.Value != "null" || c.Origin != oif.QualifiedMapping || c.Reason == "" {
		t.Fatalf("encoding change lacks accounting: %+v", c)
	}
}

func TestLowerEmbeddingUntouchedControlsStayUndeclared(t *testing.T) {
	source := testRequest(t, "openai-embeddings", "embeddings", `{"model":"route","input":"a","encoding_format":"base64"}`)
	doc, declared, err := lowerEmbedding(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay(t, source.Document(), doc, declared)
	set := byPointer(declared)
	if len(set) != 1 || set["/encoding_format"].Pointer != "" {
		t.Fatalf("unchanged controls were declared changed: %+v", declared)
	}
}

func TestLowerRerankDeclaresRelocation(t *testing.T) {
	source := testRequest(t, "rerank", "rerank", `{"model":"route","query":"q","documents":["a","b"],"top_n":2}`)
	doc, declared, err := lowerRerank(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay(t, source.Document(), doc, declared)
	set := byPointer(declared)
	removal, introduction := set["/top_n"], set["/top_k"]
	if len(set) != 2 || !removal.Remove || introduction.Remove || introduction.Value != "2" || removal.Reason == "" || removal.Reason != introduction.Reason || removal.Origin != oif.QualifiedMapping {
		t.Fatalf("selection relocation lacks paired accounting: %+v", declared)
	}
}

func TestProjectEmbeddingDeclaresResultAccounting(t *testing.T) {
	source := testRequest(t, "openai-embeddings", "embeddings", `{"model":"route","input":"a"}`)
	result := testResult(t, "embeddings", `{"object":"list","data":[{"index":0,"embedding":[1]}],"model":"vendor-model","usage":{"total_tokens":7}}`)
	doc, declared, err := projectEmbedding(source, result, nil, "route")
	if err != nil {
		t.Fatal(err)
	}
	replay(t, result.Source(), doc, declared)
	set := byPointer(declared)
	if c := set["/model"]; c.Origin != oif.IdentityBinding || c.Reason != "published model binding" {
		t.Fatalf("model binding lacks accounting: %+v", c)
	}
	if c := set["/usage/prompt_tokens"]; c.Origin != oif.QualifiedMapping || c.Value != "7" || c.Reason == "" {
		t.Fatalf("usage projection lacks accounting: %+v", c)
	}
}

func TestProjectRerankDeclaresResultRelocation(t *testing.T) {
	source := testRequest(t, "rerank", "rerank", `{"model":"route","query":"q","documents":["a"]}`)
	result := testResult(t, "rerank", `{"model":"vendor-model","data":[{"index":0,"relevance_score":0.5}],"usage":{"total_tokens":3}}`)
	doc, declared, err := projectRerank(source, result, nil, "route")
	if err != nil {
		t.Fatal(err)
	}
	replay(t, result.Source(), doc, declared)
	set := byPointer(declared)
	removal, introduction := set["/data"], set["/results"]
	if !removal.Remove || introduction.Remove || removal.Reason == "" || removal.Reason != introduction.Reason {
		t.Fatalf("result relocation lacks paired accounting: %+v", declared)
	}
	if c := set["/model"]; c.Origin != oif.IdentityBinding {
		t.Fatalf("model binding lacks accounting: %+v", c)
	}
}

// Every realized projection change must be covered by the registered
// declaration the pre-dispatch receipt advertises.
func TestRegisteredProjectionsAreDeclared(t *testing.T) {
	for _, m := range mappings() {
		var source oif.Request
		var result oif.Result
		switch m.Source.ID {
		case "openai-embeddings":
			source = testRequest(t, "openai-embeddings", "embeddings", `{"model":"route","input":"a"}`)
			result = testResult(t, "embeddings", `{"model":"vendor-model","data":[],"usage":{"total_tokens":7}}`)
		case "rerank":
			source = testRequest(t, "rerank", "rerank", `{"model":"route","query":"q","documents":["a"]}`)
			result = testResult(t, "rerank", `{"model":"vendor-model","data":[{"index":0,"relevance_score":0.5}]}`)
		default:
			t.Fatalf("untested mapping %s", m.Source.ID)
		}
		doc, declared, err := m.Project(source, result, nil, "route")
		if err != nil {
			t.Fatal(err)
		}
		replay(t, result.Source(), doc, declared)
		covered := map[string]string{}
		for _, d := range m.Projected {
			covered[d.Field] = d.Rule
		}
		for _, change := range declared {
			field := "/result" + change.Pointer
			if rule, ok := covered[field]; !ok || rule != change.Reason {
				t.Fatalf("%s realized change %s(%s) outside declared projection: %+v", m.Source.ID, field, change.Reason, m.Projected)
			}
		}
	}
}

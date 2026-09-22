package operationplan_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
)

func config(profile, operation string) operationplan.Config {
	return operationplan.Config{Provider: connectors.Config{Kind: "openai_compatible", ProfileID: profile, ProfileRevision: "1", AuthMode: "none", Endpoint: "https://example.com/v1"}, Operation: operation, Model: "vendor-model"}
}
func TestQualifiedEmbeddingControlsAndStorage(t *testing.T) {
	c := config("voyage-embeddings", "embeddings")
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("openai-embeddings", []byte(`{"model":"route","input":["a","b"],"dimensions":2,"encoding_format":"base64"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(plan.Body(), &body)
	if string(body["output_dimension"]) != "2" || string(body["truncation"]) != "false" || body["dimensions"] != nil || plan.Receipt().Class != "qualified_interaction" {
		t.Fatalf("bad mapping %s", plan.Body())
	}
	result, err := plan.Decode([]byte(`{"object":"list","data":[{"index":1,"embedding":"AACAPwAAAMA="},{"index":0,"embedding":"AACAPwAAAMA="}],"model":"vendor-model","usage":{"total_tokens":7},"native":{"counter":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(result.Body, &out)
	if string(out["model"]) != `"route"` || string(out["native"]) != `{"counter":9007199254740993}` {
		t.Fatalf("target source discarded: %s", result.Body)
	}
}
func TestNativeDefaultsPreserveSourcePresenceAndRequireClient(t *testing.T) {
	c := config("voyage-embeddings", "embeddings")
	c.Provider.OperationDefaults = map[string]connectors.DefaultSet{"embeddings": {Dialect: "voyage-embeddings", Values: map[string]json.RawMessage{"output_dtype": json.RawMessage(`"uint8"`), "truncation": json.RawMessage(`true`), "encoding_format": json.RawMessage(`"base64"`)}}}
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("voyage-embeddings", []byte(`{"model":"route","input":"a","truncation":false,"encoding_format":null,"native":{"zero":-0}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = template.Bind(source, operationplan.Context{Route: "route"}); err == nil {
		t.Fatal("missing raw-vector contract accepted")
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(plan.Body(), &fields)
	if string(fields["truncation"]) != "false" || string(fields["encoding_format"]) != "null" || string(fields["native"]) != `{"zero":-0}` {
		t.Fatalf("source rewritten %s", plan.Body())
	}
}
func TestUninspectableVectorOutputPolicyRefusesBeforeDispatch(t *testing.T) {
	c := config("tei-embeddings", "embeddings")
	c.Policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "block", Phase: "output", Action: "block", Pattern: "secret"}}}
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := operationplan.Parse("tei-embeddings", []byte(`{"inputs":"a"}`), 0)
	plan, err := template.Bind(request, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = plan.CheckInput(); err == nil {
		t.Fatal("missing output policy coverage admitted")
	}
}

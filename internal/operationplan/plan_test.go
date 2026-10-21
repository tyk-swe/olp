package operationplan_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
)

func config(profile, operation string) operationplan.Config {
	return operationplan.Config{Provider: connectors.Config{Kind: "openai_compatible", ProfileID: profile, ProfileRevision: "1", AuthMode: "none", Endpoint: "https://example.com/v1"}, Operation: operation, Model: "vendor-model"}
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

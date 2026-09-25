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
func dispositionRows(plan *operationplan.Plan) map[string][3]string {
	rows := map[string][3]string{}
	for _, d := range plan.Receipt().Dispositions {
		rows[d.Field] = [3]string{d.Disposition, d.Rule, d.Evidence}
	}
	return rows
}

func TestQualifiedReceiptAccountsEveryRequestAndResultChange(t *testing.T) {
	c := config("voyage-embeddings", "embeddings")
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("openai-embeddings", []byte(`{"model":"route","input":["a","b"],"dimensions":2,"encoding_format":"float"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	rows := dispositionRows(plan)
	want := map[string][3]string{
		"/model":            {"bound", "published model binding", "native-embedding-storage/1"},
		"/input":            {"preserved", "native_source_identity", "native-embedding-storage/1"},
		"/dimensions":       {"relocated", "qualified native control relocation", "qualified-openai-voyage-float-embedding/1"},
		"/output_dimension": {"relocated", "qualified native control relocation", "qualified-openai-voyage-float-embedding/1"},
		"/encoding_format":  {"mapped", "registered equivalent native control", "qualified-openai-voyage-float-embedding/1"},
		"/truncation":       {"introduced", "registered equivalent native control", "qualified-openai-voyage-float-embedding/1"},
		// The declared result projection is advertised before dispatch.
		"/result/model":               {"bound", "published model binding", "qualified-openai-voyage-float-embedding/1"},
		"/result/usage/prompt_tokens": {"introduced", "registered equivalent native control", "qualified-openai-voyage-float-embedding/1"},
	}
	for field, expected := range want {
		row, ok := rows[field]
		if !ok || row != expected {
			t.Fatalf("bad disposition for %s: %v (receipt %+v)", field, row, plan.Receipt().Dispositions)
		}
	}
	for field := range rows {
		if field == "/request" || field == "/" {
			t.Fatalf("blanket identity disposition survived: %+v", plan.Receipt().Dispositions)
		}
	}
	// Every non-root prepared provenance entry is accounted by a disposition
	// row with the same field and rule — receipts describe the same
	// construction the caller receives.
	byFieldAndRule := map[[2]string]bool{}
	for _, d := range plan.Receipt().Dispositions {
		byFieldAndRule[[2]string{d.Field, d.Rule}] = true
	}
	for _, entry := range plan.Prepared().Provenance() {
		if entry.Pointer == "" {
			continue
		}
		if !byFieldAndRule[[2]string{entry.Pointer, entry.Reason}] {
			t.Fatalf("provenance entry %s(%s) has no receipt disposition", entry.Pointer, entry.Reason)
		}
	}
	result, err := plan.Decode([]byte(`{"object":"list","data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}],"model":"vendor-model","usage":{"total_tokens":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	projected := map[string][2]string{}
	for _, d := range result.Projected {
		projected[d.Field] = [2]string{d.Disposition, d.Rule}
	}
	for field, expected := range map[string][2]string{
		"/result/model":               {"bound", "published model binding"},
		"/result/usage/prompt_tokens": {"introduced", "registered equivalent native control"},
	} {
		if projected[field] != expected {
			t.Fatalf("bad realized result accounting for %s: %+v", field, result.Projected)
		}
	}
}

func TestQualifiedRerankReceiptAccountsSelectionRelocation(t *testing.T) {
	c := config("voyage-rerank", "rerank")
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("rerank", []byte(`{"model":"route","query":"q","documents":["a","b"],"top_n":2}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	rows := dispositionRows(plan)
	want := map[string][3]string{
		"/model":          {"bound", "published model binding", "native-rerank-identity-scores/1"},
		"/query":          {"preserved", "native_source_identity", "native-rerank-identity-scores/1"},
		"/documents":      {"preserved", "native_source_identity", "native-rerank-identity-scores/1"},
		"/top_n":          {"relocated", "qualified native control relocation", "qualified-rerank-voyage-selection/1"},
		"/top_k":          {"relocated", "qualified native control relocation", "qualified-rerank-voyage-selection/1"},
		"/result/model":   {"bound", "published model binding", "qualified-rerank-voyage-selection/1"},
		"/result/data":    {"relocated", "qualified native control relocation", "qualified-rerank-voyage-selection/1"},
		"/result/results": {"relocated", "qualified native control relocation", "qualified-rerank-voyage-selection/1"},
	}
	for field, expected := range want {
		row, ok := rows[field]
		if !ok || row != expected {
			t.Fatalf("bad disposition for %s: %v (receipt %+v)", field, row, plan.Receipt().Dispositions)
		}
	}
	if _, ok := rows["/request"]; ok {
		t.Fatalf("blanket identity disposition survived: %+v", plan.Receipt().Dispositions)
	}
	result, err := plan.Decode([]byte(`{"model":"vendor-model","data":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.25}],"usage":{"total_tokens":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	projected := map[string][2]string{}
	for _, d := range result.Projected {
		projected[d.Field] = [2]string{d.Disposition, d.Rule}
	}
	for field, expected := range map[string][2]string{
		"/result/model":   {"bound", "published model binding"},
		"/result/data":    {"relocated", "qualified native control relocation"},
		"/result/results": {"relocated", "qualified native control relocation"},
	} {
		if projected[field] != expected {
			t.Fatalf("bad realized result accounting for %s: %+v", field, result.Projected)
		}
	}
}

func TestNativeReceiptPreservesMembersPerField(t *testing.T) {
	c := config("voyage-embeddings", "embeddings")
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("voyage-embeddings", []byte(`{"model":"route","input":"a","truncation":true,"private_marker":{"x":1}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient})
	if err != nil {
		t.Fatal(err)
	}
	rows := dispositionRows(plan)
	want := map[string][3]string{
		"/model":      {"bound", "published model binding", "native-embedding-storage/1"},
		"/input":      {"preserved", "native_source_identity", "native-embedding-storage/1"},
		"/truncation": {"preserved", "native_source_identity", "native-embedding-storage/1"},
		"/$native":    {"preserved", "native_source_identity", "native-embedding-storage/1"},
	}
	for field, expected := range want {
		row, ok := rows[field]
		if !ok || row != expected {
			t.Fatalf("bad disposition for %s: %v (receipt %+v)", field, row, plan.Receipt().Dispositions)
		}
	}
	for field := range rows {
		if field == "/request" || field == "/private_marker" {
			t.Fatalf("blanket or caller-named disposition leaked: %+v", plan.Receipt().Dispositions)
		}
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

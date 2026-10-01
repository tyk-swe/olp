package rerank_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/rerank"
)

func definition(t *testing.T, id string) operations.Dialect {
	t.Helper()
	for _, d := range rerank.Definitions() {
		if d.Identity.ID == id {
			return d
		}
	}
	t.Fatal("missing dialect")
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
	return r
}
func result(t *testing.T, r oif.Request, raw string) oif.Result {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := oif.NewResult(r.Descriptor(), doc, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOutputPolicyInspectsObjectDocuments(t *testing.T) {
	d := definition(t, "rerank")
	r := request(t, d, `{"model":"m","query":"q","documents":["one","two"],"return_documents":true}`)
	native := result(t, r, `{"results":[{"index":1,"relevance_score":0.9,"document":{"text":"two"}},{"index":0,"relevance_score":0.1,"document":{"text":"one"}}]}`)
	if _, err := d.Result(r, native); err != nil {
		t.Fatal(err)
	}
	texts, err := d.OutputText(native)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, text := range texts {
		got[text.Pointer] = text.Value
	}
	if got["/results/0/document/text"] != "two" || got["/results/1/document/text"] != "one" {
		t.Fatalf("object document text was not inspected: %+v", texts)
	}
	nested := result(t, r, `{"results":[{"index":1,"relevance_score":0.9,"document":{"text":"two","meta":{"x":1}}}]}`)
	if _, err := d.OutputText(nested); err == nil {
		t.Fatal("nested non-string document member was not refused")
	} else if safe, ok := err.(interface {
		Incompatibility() (string, string, string, string)
	}); !ok {
		t.Fatalf("unscoped error: %v", err)
	} else if code, _, _, _ := safe.Incompatibility(); code != "policy_conflict" {
		t.Fatalf("code %s, expected policy_conflict", code)
	}
}

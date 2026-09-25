package operationplan

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func testDocument(t *testing.T, raw string) oif.Document {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 1 << 20, MaxNodes: 1 << 12, MaxDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func apply(t *testing.T, doc oif.Document, changes []oif.Change) oif.Document {
	t.Helper()
	out, err := oif.Apply(doc, changes)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The destination and its declared changes are one construction: removing a
// declared change while the destination keeps it, changing a declared value,
// or inventing a declaration the destination does not have all fail.
func TestDeclaredConstructionRejectsMutation(t *testing.T) {
	source := testDocument(t, `{"model":"route","dimensions":2}`)
	relocate := []oif.Change{
		{Pointer: "/dimensions", Remove: true, Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
		{Pointer: "/output_dimension", Value: "2", Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
	}
	destination := apply(t, source, relocate)
	if err := declaredConstruction(source, destination, relocate, "/request"); err != nil {
		t.Fatalf("exact construction rejected: %v", err)
	}
	// A declared disposition missing: the destination still carries the
	// removal/introduction but the declaration omits the removal half.
	if err := declaredConstruction(source, destination, relocate[1:], "/request"); err == nil {
		t.Fatal("destination change without declared provenance accepted")
	}
	// A declared value that differs from the constructed destination.
	mutated := append([]oif.Change{}, relocate...)
	mutated[1].Value = "3"
	if err := declaredConstruction(source, destination, mutated, "/request"); err == nil {
		t.Fatal("declared value differing from construction accepted")
	}
	// A declaration the destination never realized.
	invented := append(append([]oif.Change{}, relocate...), oif.Change{Pointer: "/truncation", Value: "false", Origin: oif.QualifiedMapping, Reason: "invented"})
	if err := declaredConstruction(source, destination, invented, "/request"); err == nil {
		t.Fatal("invented declared change accepted")
	}
	// A declaration without provenance vocabulary.
	unscoped := []oif.Change{{Pointer: "/truncation", Value: "false"}}
	if err := declaredConstruction(source, destination, unscoped, "/request"); err == nil {
		t.Fatal("unscoped declared change accepted")
	}
	// An empty declaration admits only an untouched destination.
	if err := declaredConstruction(source, source, nil, "/request"); err != nil {
		t.Fatalf("identity construction rejected: %v", err)
	}
	if err := declaredConstruction(source, destination, nil, "/request"); err == nil {
		t.Fatal("undeclared construction accepted")
	}
}

// Result projections are declared per-field: a realized change absent from
// the declaration, or with a different rule identity, is rejected.
func TestDeclaredProjectionRejectsUndeclaredResultChange(t *testing.T) {
	declared := []oif.Disposition{
		{Field: "/result/model", Disposition: "bound", Rule: "published model binding"},
		{Field: "/result/data", Disposition: "relocated", Rule: "qualified native control relocation"},
		{Field: "/result/results", Disposition: "relocated", Rule: "qualified native control relocation"},
	}
	realized := []oif.Provenance{
		{Pointer: "/model", Origin: oif.IdentityBinding, Reason: "published model binding"},
		{Pointer: "/data", Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
		{Pointer: "/results", Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
	}
	if err := declaredProjection(declared, realized); err != nil {
		t.Fatalf("declared projection rejected: %v", err)
	}
	if err := declaredProjection(declared[:1], realized); err == nil {
		t.Fatal("realized relocation outside the declaration accepted")
	}
	wrongRule := []oif.Disposition{{Field: "/result/model", Disposition: "bound", Rule: "other rule"}}
	if err := declaredProjection(wrongRule, realized[:1]); err == nil {
		t.Fatal("realized change under an undeclared rule accepted")
	}
	if err := declaredProjection(declared, nil); err != nil {
		t.Fatal("empty realization rejected")
	}
}

func TestMemberDispositionsAccountPreservedAndChanged(t *testing.T) {
	source := testDocument(t, `{"model":"route","input":["a"],"dimensions":2,"mystery":{"x":1}}`)
	changes := []oif.Change{
		{Pointer: "/dimensions", Remove: true, Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
		{Pointer: "/output_dimension", Value: "2", Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
		{Pointer: "/truncation", Value: "false", Origin: oif.QualifiedMapping, Reason: "registered equivalent native control"},
		{Pointer: "/model", Value: `"bound"`, Origin: oif.IdentityBinding, Reason: "published model binding"},
	}
	destination := apply(t, source, changes)
	vocab := map[string]bool{"model": true, "input": true, "dimensions": true, "output_dimension": true, "truncation": true}
	rows := memberDispositions(source, destination, changeProvenance(changes), vocab, "codec-evidence", "mapping-evidence")
	byField := map[string]oif.Disposition{}
	for _, row := range rows {
		byField[row.Field] = row
	}
	want := map[string][3]string{
		"/model":            {"bound", "published model binding", "codec-evidence"},
		"/input":            {"preserved", "native_source_identity", "codec-evidence"},
		"/dimensions":       {"relocated", "qualified native control relocation", "mapping-evidence"},
		"/output_dimension": {"relocated", "qualified native control relocation", "mapping-evidence"},
		"/truncation":       {"introduced", "registered equivalent native control", "mapping-evidence"},
		"/$native":          {"preserved", "native_source_identity", "codec-evidence"},
	}
	if len(rows) != len(want) {
		t.Fatalf("unexpected dispositions: %+v", rows)
	}
	for field, expected := range want {
		row, ok := byField[field]
		if !ok || row.Disposition != expected[0] || row.Rule != expected[1] || row.Evidence != expected[2] {
			t.Fatalf("bad disposition for %s: %+v (all: %+v)", field, row, rows)
		}
	}
	for _, row := range rows {
		if row.Field == "/request" || row.Field == "/mystery" {
			t.Fatalf("blanket or arbitrary-name disposition leaked: %+v", rows)
		}
	}
}

func TestResultDispositionsScopeAndKinds(t *testing.T) {
	source := testDocument(t, `{"model":"vendor","data":[1],"usage":{"total_tokens":7}}`)
	changes := []oif.Change{
		{Pointer: "/model", Value: `"route"`, Origin: oif.IdentityBinding, Reason: "published model binding"},
		{Pointer: "/data", Remove: true, Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
		{Pointer: "/results", Value: `[1]`, Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
		{Pointer: "/usage/prompt_tokens", Value: "7", Origin: oif.QualifiedMapping, Reason: "registered equivalent native control"},
	}
	output := apply(t, source, changes)
	rows := resultDispositions(source, output, changeProvenance(changes), "codec-evidence", "mapping-evidence")
	byField := map[string]oif.Disposition{}
	for _, row := range rows {
		byField[row.Field] = row
	}
	want := map[string][3]string{
		"/result/model":               {"bound", "published model binding", "codec-evidence"},
		"/result/data":                {"relocated", "qualified native control relocation", "mapping-evidence"},
		"/result/results":             {"relocated", "qualified native control relocation", "mapping-evidence"},
		"/result/usage/prompt_tokens": {"introduced", "registered equivalent native control", "mapping-evidence"},
	}
	if len(rows) != len(want) {
		t.Fatalf("unexpected result dispositions: %+v", rows)
	}
	for field, expected := range want {
		row, ok := byField[field]
		if !ok || row.Disposition != expected[0] || row.Rule != expected[1] || row.Evidence != expected[2] {
			t.Fatalf("bad result disposition for %s: %+v", field, row)
		}
	}
}

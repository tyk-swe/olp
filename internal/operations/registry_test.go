package operations_test

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func testDialect(id string) operations.Dialect {
	return operations.Dialect{
		Identity:  oif.Identity{ID: id, Revision: "1"},
		Operation: oif.Identity{ID: "embeddings", Revision: "1"},
		Label:     id,
		Address:   operations.Address{LegacyPath: "probe"},
		Request:   func(oif.Request) (oif.View, error) { return nil, nil },
		Result:    func(oif.Request, oif.Result) (oif.View, error) { return nil, nil },
		Probe:     func(string) []byte { return []byte(`{}`) },
		Evidence:  "test-dialect/1",
	}
}

func testMapping() operations.Mapping {
	return operations.Mapping{
		Source: oif.Identity{ID: "a", Revision: "1"},
		Target: oif.Identity{ID: "b", Revision: "1"},
		Lower: func(oif.Request, oif.View) (oif.Document, []oif.Change, error) {
			return oif.Document{}, nil, nil
		},
		Project: func(oif.Request, oif.Result, oif.View, string) (oif.Document, []oif.Change, error) {
			return oif.Document{}, nil, nil
		},
		Evidence: "test-mapping/1",
	}
}

// The declared result projection is a bounded, /result-scoped schema
// vocabulary contract — incomplete declarations are refused at registration.
func TestRegisterMappingValidatesResultDeclaration(t *testing.T) {
	registry := operations.NewRegistry()
	if err := registry.Register(testDialect("a")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(testDialect("b")); err != nil {
		t.Fatal(err)
	}
	m := testMapping()
	m.Projected = []oif.Disposition{{Field: "/result/model", Disposition: "bound", Rule: "published model binding"}}
	if err := registry.RegisterMapping(m); err != nil {
		t.Fatalf("complete declaration rejected: %v", err)
	}
	bad := []oif.Disposition{
		{Field: "/request/model", Disposition: "bound", Rule: "published model binding"},
		{Field: "/result/model", Disposition: "", Rule: "published model binding"},
		{Field: "/result/model", Disposition: "bound"},
		{Field: "/result/" + strings.Repeat("x", 300), Disposition: "bound", Rule: "published model binding"},
	}
	for i, disposition := range bad {
		registry := operations.NewRegistry()
		_ = registry.Register(testDialect("a"))
		_ = registry.Register(testDialect("b"))
		m := testMapping()
		m.Projected = []oif.Disposition{disposition}
		if err := registry.RegisterMapping(m); err == nil {
			t.Fatalf("invalid declaration %d registered", i)
		}
	}
	registry = operations.NewRegistry()
	_ = registry.Register(testDialect("a"))
	_ = registry.Register(testDialect("b"))
	m = testMapping()
	for len(m.Projected) <= 64 {
		m.Projected = append(m.Projected, oif.Disposition{Field: "/result/model", Disposition: "bound", Rule: "r"})
	}
	if err := registry.RegisterMapping(m); err == nil {
		t.Fatal("unbounded declaration registered")
	}
}

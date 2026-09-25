package providers

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/generation"
)

// The catalog reports the dialect's declared identity rule: a generation
// grammar that binds /model is required, everything else is none — the
// console must never guess from a dialect id.
func TestGenerationModelBindingFollowsIdentityRules(t *testing.T) {
	bound := generation.Dialect{IdentityRules: []oif.IdentityRule{
		{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String},
	}}
	if got := generationModelBinding(bound); got != string(operations.ModelRequired) {
		t.Fatalf("model-bound generation dialect reported %q", got)
	}
	unbound := generation.Dialect{IdentityRules: []oif.IdentityRule{
		{Pointer: "/metadata/session", Origin: oif.IdentityBinding, Kind: oif.String},
	}}
	if got := generationModelBinding(unbound); got != string(operations.ModelNone) {
		t.Fatalf("addressing-bound generation dialect reported %q", got)
	}
}

// Every catalog row must carry a declared model binding that agrees with its
// registered binding contract; a mismatch would have the console inject or
// withhold a body model against the dialect's own rules.
func TestOperationDialectCatalogCarriesDeclaredModelBinding(t *testing.T) {
	if len(operationregistry.Default.Dialects()) == 0 {
		t.Fatal("no unary dialects registered")
	}
	for _, d := range operationregistry.Default.Dialects() {
		switch d.ModelBinding {
		case operations.ModelRequired, operations.ModelOptional, operations.ModelNone:
		default:
			t.Fatalf("dialect %s carries undeclared model binding %q", d.Identity.ID, d.ModelBinding)
		}
		if (d.BindModel != nil) != (d.ModelBinding != operations.ModelNone) {
			t.Fatalf("dialect %s model binding %q contradicts its binding contract", d.Identity.ID, d.ModelBinding)
		}
	}
	if len(operationregistry.Generation.Dialects()) == 0 {
		t.Fatal("no generation dialects registered")
	}
	for _, d := range operationregistry.Generation.Dialects() {
		if binding := generationModelBinding(d); binding != string(operations.ModelRequired) && binding != string(operations.ModelNone) {
			t.Fatalf("generation dialect %s reported %q", d.Identity.ID, binding)
		}
	}
}

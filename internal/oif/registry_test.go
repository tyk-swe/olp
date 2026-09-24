package oif_test

import (
	"fmt"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

// This fixture operation knows only its own shape, not generation or a
// provider. Two independently registered wire adapters interpret native names.
var shapeOperation = oif.Identity{ID: "fixture_tensor_shape", Revision: "1"}

type shapeView struct{ dimensions []string }

func (shapeView) Schema() oif.Identity { return shapeOperation }

type shapeDialect struct{ field string }

func (d shapeDialect) LiftRequest(r oif.Request) (oif.View, error) {
	v, ok := r.Document().Root().Lookup(d.field)
	if !ok || v.Kind() != oif.Array {
		return nil, fmt.Errorf("missing native shape")
	}
	shape := shapeView{}
	for _, element := range v.Elements() {
		if element.Kind() != oif.Number {
			return nil, fmt.Errorf("invalid dimension")
		}
		shape.dimensions = append(shape.dimensions, element.Raw())
	}
	return shape, nil
}
func TestIndependentOperationAndDialectLinking(t *testing.T) {
	first := oif.Identity{ID: "fixture-tensor-a", Revision: "1"}
	second := oif.Identity{ID: "fixture-tensor-b", Revision: "2"}
	registry, err := oif.NewRegistry([]oif.Operation{{Identity: shapeOperation}}, []oif.Binding{{Dialect: first, Operation: shapeOperation, Request: shapeDialect{"shape"}}, {Dialect: second, Operation: shapeOperation, Request: shapeDialect{"dimensions"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		dialect oif.Identity
		raw     string
	}{{first, `{"shape":[9007199254740993,4],"native":null}`}, {second, `{"dimensions":[9007199254740993,4],"native":null}`}} {
		r, err := registry.Request(oif.Descriptor{Dialect: test.dialect, Operation: shapeOperation}, source(t, test.raw))
		if err != nil {
			t.Fatal(err)
		}
		binding, err := registry.Binding(test.dialect, shapeOperation)
		if err != nil {
			t.Fatal(err)
		}
		view, err := binding.Request.LiftRequest(r)
		if err != nil {
			t.Fatal(err)
		}
		if view.Schema() != shapeOperation || view.(shapeView).dimensions[0] != "9007199254740993" {
			t.Fatal("operation was coerced through a generation representation")
		}
		p, err := registry.PrepareIdentity(r, r.Descriptor(), nil)
		if err != nil || p.Document().Raw() != test.raw {
			t.Fatal("registered native operation did not prepare unchanged", err)
		}
	}
	if _, err := registry.Request(oif.Descriptor{Dialect: first, Operation: oif.Identity{ID: "unregistered", Revision: "1"}}, source(t, `{}`)); err == nil {
		t.Fatal("unlinked operation accepted")
	}
}

func TestBlobReferenceRetainsAuthorityWithoutOwningStorage(t *testing.T) {
	ref, err := oif.NewBlobReference("existing-media-object", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "application/octet-stream", 9007199254740993)
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewBlobRequest(oif.Descriptor{Operation: shapeOperation, Dialect: oif.Identity{ID: "fixture-binary", Revision: "1"}}, ref)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source().Size() != 9007199254740993 || r.Source().ID() != "existing-media-object" {
		t.Fatal("blob reference lost identity/size")
	}
}

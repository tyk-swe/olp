package oif_test

import (
	"fmt"

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

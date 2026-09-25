package interaction

import (
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/generation"
)

// defaultRegistry is the built-in generation registry. Tests may inject a
// narrower registry through Config.Registry; providers can never supply one.
func defaultRegistry() *generation.Registry { return operationregistry.Generation }

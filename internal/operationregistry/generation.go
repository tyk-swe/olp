package operationregistry

import (
	"github.com/tyk-swe/olp/internal/operations/generation"
)

// Generation is the built-in generation registry: registered dialect
// contracts resolved by identity or connector label without family switches in
// generic orchestration. The registry owns only contract values; the codec
// packages that implement them register through RegisterGenerationDialects
// from their own init, so this composition root never imports a provider codec
// and provider configuration can never install one.
var Generation = generation.NewRegistry()

// RegisterGenerationDialects publishes built-in generation dialects and their
// qualified mappings into Generation. Protocol registrations call it during
// package init; a registration defect is a build defect and panics.
func RegisterGenerationDialects(dialects []generation.Dialect, mappings []generation.Mapping) {
	for _, dialect := range dialects {
		if err := Generation.Register(dialect); err != nil {
			panic(err)
		}
	}
	for _, mapping := range mappings {
		if err := Generation.RegisterMapping(mapping); err != nil {
			panic(err)
		}
	}
}

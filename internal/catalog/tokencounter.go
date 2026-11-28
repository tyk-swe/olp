package catalog

import (
	"sync"

	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
)

// A process refuses to start with a catalog that does not verify, so only a tool
// that cannot verify it reaches the empty factors.
var familyFactors = sync.OnceValue(func() map[estimate.Family]float64 {
	factors := map[estimate.Family]float64{}
	estimation, err := EmbeddedEstimation()
	if err != nil {
		return factors
	}
	for _, e := range estimation {
		if factor, err := e.Factor.Float64(); err == nil {
			factors[estimate.Family(e.Family)] = factor
		}
	}
	return factors
})

// TokenCounter returns the counter for an upstream model name, scaled by the
// factors this release's catalog measured.
func TokenCounter(model string) estimate.Counter {
	return estimate.ForModel(model, familyFactors())
}

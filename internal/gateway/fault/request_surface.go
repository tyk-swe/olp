package fault

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/surface"
)

// RequestSurface names the inference API a gateway request speaks.
func RequestSurface(r *http.Request) string {
	if classified := surface.Of(r.URL.Path); classified.Inference {
		return classified.Name
	}
	return "openai"
}

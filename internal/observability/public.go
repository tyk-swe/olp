package observability

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/surface"
)

// RetryAfterOverload is the retry hint every admission rejection carries.
const RetryAfterOverload = "1"

// PublicAdmission bounds the whole public listener before routing: every
// request is charged against its surface's pool, and a full pool rejects
// unless its queue has room. Inference surfaces take the inference pool and every
// other public request takes the management pool, so a flood of console or
// management calls cannot starve inference, and vice versa. The permit rides
// the request context so inner handlers can await a queued slot once they know
// the request's priority, or release it early; the final release runs when the
// handler returns.
type PublicAdmission struct {
	Inference  *Pool
	Management *Pool
	// InferenceEnabled reports whether this process serves inference at all;
	// without it every request is management surface.
	InferenceEnabled bool
	// Reject writes the surface's overload response.
	Reject func(w http.ResponseWriter, r *http.Request, surface string)
}

func (a *PublicAdmission) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		classified := surface.Of(r.URL.Path)
		pool := a.Management
		if classified.Inference && a.InferenceEnabled {
			pool = a.Inference
		}
		permit := pool.Enter()
		if permit == nil {
			w.Header().Set("Retry-After", RetryAfterOverload)
			a.Reject(w, r, classified.Name)
			return
		}
		defer permit.Release()
		next.ServeHTTP(w, r.WithContext(WithPermit(r.Context(), permit)))
	})
}

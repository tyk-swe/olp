package observability

import (
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/tyk-swe/olp/internal/surface"
	"github.com/tyk-swe/olp/internal/telemetry"
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
	// Tracing is the request tracing configuration, nil when no exporter is
	// installed. Tracer is the process tracer (a no-op when tracing is off).
	Tracing *telemetry.RequestConfig
	Tracer  trace.Tracer
}

// Wrap returns next behind admission and, when configured, request tracing.
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
		handler := next
		if a.Tracing != nil {
			handler = telemetry.AdmittedRequest(*a.Tracing, a.Tracer, classified.Name, handler)
		}
		handler.ServeHTTP(w, r.WithContext(WithPermit(r.Context(), permit)))
	})
}

package geminiinteractions

import (
	"net/http"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/routing"
)

const profile = "gemini-interactions"

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	s := surface{kernel: k}
	mux.HandleFunc("POST /gemini/v1beta/interactions", s.create)
	mux.HandleFunc("GET /gemini/v1beta/interactions/{id}", s.resource)
	mux.HandleFunc("DELETE /gemini/v1beta/interactions/{id}", s.resource)
	mux.HandleFunc("POST /gemini/v1beta/interactions/{id}/cancel", s.resource)
}

// A turn's pin, status and outcome are assigned in place because the deferred
// settlement must read them even when a step panics.
type turn struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	pin       *kernel.Pin
	status    int
	out       *kernel.Outcome
}

func (s surface) begin(w http.ResponseWriter, r *http.Request, stored bool) (*turn, bool) {
	x, authority, ok := s.kernel.Enter(w, r, wire.GeminiInteractions, stored, func(r *http.Request) (keyauthority.Authority, *fault.Error) {
		if _, e := kernel.GeminiClientKey(r, "stream", "last_event_id", "include_input"); e != nil {
			return keyauthority.Authority{}, e
		}
		return s.kernel.Gate.Authenticate(r, "inference")
	})
	if !ok {
		return nil, false
	}
	return &turn{surface: s, w: w, r: r, x: x, authority: authority, status: http.StatusInternalServerError}, true
}

func (t *turn) refuse(e *fault.Error) {
	t.x.Failure, t.status = e, e.Status
	if !t.x.ResourceCall() && t.x.Dispatched() && t.x.Strict() && e.Status >= 500 {
		final := *e
		final.NoRetry = true
		fault.WriteSurfaceError(t.w, &final, "gemini")
		return
	}
	fault.WriteSurfaceError(t.w, e, "gemini")
}

func (t *turn) supports(provider *routing.Provider, model string) bool {
	return provider.ProfileID == profile && provider.Supports(model, "generation", "gemini", t.x.Mode())
}

func (s surface) stateConfigured() bool {
	return s.kernel.Deps.Resources != nil && s.kernel.Deps.Resources.Encrypted() && s.kernel.Deps.Resolver != nil
}

func (s surface) responseLimit() int64 {
	if limit := s.kernel.Config.MaxResponseBytes; limit > 0 {
		return limit
	}
	return 16 << 20
}

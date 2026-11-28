package geminiinteractions

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

const profile = "gemini-interactions"

type surface struct {
	kernel    *kernel.Server
	store     *resources.Store
	resolver  *resources.Resolver
	admission snapshotInstaller
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store, resolver *resources.Resolver, admission snapshotInstaller) {
	s := surface{kernel: k, store: store, resolver: resolver, admission: admission}
	mux.HandleFunc("POST /gemini/v1beta/interactions", s.create)
	mux.HandleFunc("GET /gemini/v1beta/interactions/{id}", s.resource)
	mux.HandleFunc("DELETE /gemini/v1beta/interactions/{id}", s.resource)
	mux.HandleFunc("POST /gemini/v1beta/interactions/{id}/cancel", s.resource)
}

type turn struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	end       *turnEnd
}

// turnEnd is the one thing a turn writes after it is built. The deferred
// settlement reads it even when a step panics.
type turnEnd struct {
	pin    *kernel.Pin
	status int
	out    *kernel.Outcome
}

func (s surface) begin(w http.ResponseWriter, r *http.Request, stored bool) (turn, bool) {
	x, authority, ok := s.kernel.Enter(w, r, wire.GeminiInteractions, stored, func(r *http.Request) (keyauthority.Authority, *fault.Error) {
		if _, e := ingress.GeminiClientKey(r, "stream", "last_event_id", "include_input"); e != nil {
			return keyauthority.Authority{}, e
		}
		return s.kernel.Gate.Authenticate(r, "inference")
	})
	if !ok {
		return turn{}, false
	}
	return turn{surface: s, w: w, r: r, x: x, authority: authority, end: &turnEnd{status: http.StatusInternalServerError}}, true
}

func (t turn) refuse(e *fault.Error) {
	t.x.Failure, t.end.status = e, e.Status
	if !t.x.ResourceCall() && t.x.Dispatched() && t.x.Strict() && e.Status >= 500 {
		final := *e
		final.NoRetry = true
		fault.WriteSurfaceError(t.w, &final, "gemini")
		return
	}
	fault.WriteSurfaceError(t.w, e, "gemini")
}

func (t turn) supports(provider *routing.Provider, model string) bool {
	return provider.ProfileID == profile && provider.Supports(model, "generation", "gemini", t.x.Mode())
}

func (s surface) stateConfigured() bool {
	return s.store != nil && s.store.Encrypted() && s.resolver != nil
}

func (s surface) responseLimit() int64 {
	if limit := s.kernel.Config.MaxResponseBytes; limit > 0 {
		return limit
	}
	return 16 << 20
}

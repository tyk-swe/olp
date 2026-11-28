package retrieval

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
)

type surface struct {
	kernel *kernel.Server
	store  *resources.Store
}

func Handler(k *kernel.Server, store *resources.Store) http.HandlerFunc {
	return surface{kernel: k, store: store}.query
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store) {
	if store == nil {
		return
	}
	mux.HandleFunc("POST /v1/retrieval/{binding_id}/query", Handler(k, store))
}

type turn struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	end       *turnEnd
}

type turnEnd struct {
	pin    *kernel.Pin
	status int
}

func (s surface) begin(w http.ResponseWriter, r *http.Request) (turn, bool) {
	x, authority, done := s.kernel.EnterRetrieval(w, r)
	if done {
		return turn{}, false
	}
	return turn{surface: s, w: w, r: r, x: x, authority: authority, end: &turnEnd{status: http.StatusInternalServerError}}, true
}

func (t turn) refuse(e *fault.Error) {
	t.x.Failure = e
	t.end.status = e.Status
	fault.WriteSurfaceError(t.w, e, "openai")
}

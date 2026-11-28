package realtime

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
)

type surface struct {
	kernel *kernel.Server
	calls  *callRegistry
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store) {
	s := surface{kernel: k, calls: &callRegistry{sessions: map[string]*webrtcSession{}}}
	if store != nil {
		mux.HandleFunc("GET /v1/realtime", s.realtime)
	}
	mux.HandleFunc("POST /v1/realtime/calls", s.realtimeCallsCreate)
}

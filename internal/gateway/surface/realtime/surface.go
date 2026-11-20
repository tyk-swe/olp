package realtime

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type surface struct {
	kernel *kernel.Server
	calls  *callRegistry
}

func Register(k *kernel.Server, mux *http.ServeMux) {
	s := surface{kernel: k, calls: &callRegistry{sessions: map[string]*webrtcSession{}}}
	if k.Deps.Resources != nil {
		mux.HandleFunc("GET /v1/realtime", s.realtime)
	}
	mux.HandleFunc("POST /v1/realtime/calls", s.realtimeCallsCreate)
}

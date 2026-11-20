package geminilive

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	s := surface{kernel: k}
	live := connectors.GeminiLiveMethod
	mux.HandleFunc("GET /gemini/ws/"+live, s.live)
	mux.HandleFunc("GET /ws/"+live, s.live)
	mux.HandleFunc("OPTIONS /gemini/ws/", k.Preflight)
}

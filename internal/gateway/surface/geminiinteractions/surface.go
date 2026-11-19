package geminiinteractions

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	s := surface{kernel: k}
	mux.HandleFunc("POST /gemini/v1beta/interactions", s.geminiInteractionCreate)
	mux.HandleFunc("GET /gemini/v1beta/interactions/{id}", s.geminiInteractionResource)
	mux.HandleFunc("DELETE /gemini/v1beta/interactions/{id}", s.geminiInteractionResource)
	mux.HandleFunc("POST /gemini/v1beta/interactions/{id}/cancel", s.geminiInteractionResource)
}

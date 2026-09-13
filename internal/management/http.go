package management

import (
	"net/http"

	"github.com/tyk-swe/olp/openapi"
)

func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(openapi.Document)
	})
	mux.HandleFunc("/api/v3/", Unimplemented)
}

func Unimplemented(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotImplemented)
	w.Write([]byte(`{"error":{"code":"not_implemented","message":"This operation is not implemented in the Go foundation."}}`))
}

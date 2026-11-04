package management

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	client "github.com/tyk-swe/olp/management"
)

func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(client.Document)
	})
	mux.HandleFunc("/api/v1/", notFound)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	access.WriteProblem(w, access.Fail(404, "operation_not_found", "The requested management operation does not exist."))
}

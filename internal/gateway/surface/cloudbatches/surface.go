package cloudbatches

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
)

type surface struct {
	kernel *kernel.Server
	store  *resources.Store
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store) {
	if store == nil {
		return
	}
	s := surface{kernel: k, store: store}
	mux.HandleFunc("POST /native/{dialect}/batch-jobs", s.create)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs", s.list)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs/{id}", s.get)
	mux.HandleFunc("POST /native/{dialect}/batch-jobs/{id}/cancel", s.cancel)
}

func (s surface) fail(x *kernel.Call, w http.ResponseWriter, e *fault.Error) {
	s.kernel.RefuseOn(x, w, e, "openai")
}

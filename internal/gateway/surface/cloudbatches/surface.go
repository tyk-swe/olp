package cloudbatches

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	if k.Deps.Resources == nil {
		return
	}
	s := surface{kernel: k}
	mux.HandleFunc("POST /native/{dialect}/batch-jobs", s.create)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs", s.list)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs/{id}", s.get)
	mux.HandleFunc("POST /native/{dialect}/batch-jobs/{id}/cancel", s.cancel)
}

func (s surface) fail(x *kernel.Call, w http.ResponseWriter, e *fault.Error) {
	s.kernel.RefuseOn(x, w, e, "openai")
}

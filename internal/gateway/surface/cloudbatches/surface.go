package cloudbatches

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	if k.Deps.Resources == nil {
		return
	}
	s := surface{kernel: k}
	mux.HandleFunc("POST /native/{dialect}/batch-jobs", s.createCloudBatch)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs", s.listCloudBatches)
	mux.HandleFunc("GET /native/{dialect}/batch-jobs/{id}", s.getCloudBatch)
	mux.HandleFunc("POST /native/{dialect}/batch-jobs/{id}/cancel", s.cancelCloudBatch)
}

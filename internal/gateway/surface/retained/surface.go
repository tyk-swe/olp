package retained

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type surface struct {
	kernel         *kernel.Server
	retrievalQuery http.HandlerFunc
}

func Register(k *kernel.Server, mux *http.ServeMux, retrievalQuery http.HandlerFunc) {
	if k.Deps.Resources == nil {
		return
	}
	s := surface{kernel: k, retrievalQuery: retrievalQuery}
	s.registerRetainedLifecycle(mux)
	s.registerRetainedGeneric(mux)
}

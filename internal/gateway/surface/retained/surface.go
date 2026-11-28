package retained

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/resources"
)

type surface struct {
	kernel         *kernel.Server
	store          *resources.Store
	admission      *quota.Admission
	retrievalQuery http.HandlerFunc
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store, admission *quota.Admission, retrievalQuery http.HandlerFunc) {
	if store == nil {
		return
	}
	s := surface{kernel: k, store: store, admission: admission, retrievalQuery: retrievalQuery}
	s.registerRetainedLifecycle(mux)
	s.registerRetainedGeneric(mux)
}

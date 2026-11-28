package anthropicstate

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
)

type surface struct {
	kernel *kernel.Server
	store  *resources.Store
	spool  *staging.Spool
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store, spool *staging.Spool) {
	if store == nil || spool == nil {
		return
	}
	s := surface{kernel: k, store: store, spool: spool}
	mux.HandleFunc("POST /anthropic/v1/files", s.uploadFile)
	mux.HandleFunc("GET /anthropic/v1/files", s.listFiles)
	mux.HandleFunc("GET /anthropic/v1/files/{id}", s.getFile)
	mux.HandleFunc("DELETE /anthropic/v1/files/{id}", s.deleteFile)
	mux.HandleFunc("GET /anthropic/v1/files/{id}/content", s.fileContent)
	mux.HandleFunc("POST /anthropic/v1/messages/batches", s.createBatch)
	mux.HandleFunc("GET /anthropic/v1/messages/batches", s.listBatches)
	mux.HandleFunc("GET /anthropic/v1/messages/batches/{id}", s.getBatch)
	mux.HandleFunc("DELETE /anthropic/v1/messages/batches/{id}", s.deleteBatch)
	mux.HandleFunc("POST /anthropic/v1/messages/batches/{id}/cancel", s.cancelBatch)
	mux.HandleFunc("GET /anthropic/v1/messages/batches/{id}/results", s.batchResults)
}

func (s surface) begin(w http.ResponseWriter, r *http.Request, family wire.Format) (*kernel.Call, keyauthority.Authority, bool) {
	x, authority, done := s.kernel.StateBegin(w, r, family)
	if done {
		return x, authority, true
	}
	if err := validateFilesBeta(r.Header); err != nil {
		s.kernel.Gate.Release(r.Context())
		s.kernel.Refuse(x, w, err)
		return x, authority, true
	}
	return x, authority, false
}

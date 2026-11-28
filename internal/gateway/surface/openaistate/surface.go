package openaistate

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
)

type surface struct {
	kernel  *kernel.Server
	store   *resources.Store
	spool   *staging.Spool
	batches *batches.Store
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store, spool *staging.Spool, batchStore *batches.Store) {
	if store == nil {
		return
	}
	s := surface{kernel: k, store: store, spool: spool, batches: batchStore}
	if spool != nil {
		mux.HandleFunc("POST /v1/files", s.uploadFile)
		mux.HandleFunc("GET /v1/files", s.listFiles)
		mux.HandleFunc("GET /v1/files/{id}", s.getFile)
		mux.HandleFunc("DELETE /v1/files/{id}", s.deleteFile)
		mux.HandleFunc("GET /v1/files/{id}/content", s.fileContent)
		mux.HandleFunc("POST /v1/batches", s.createBatch)
		mux.HandleFunc("GET /v1/batches", s.listBatches)
		mux.HandleFunc("GET /v1/batches/{id}", s.getBatch)
		mux.HandleFunc("POST /v1/batches/{id}/cancel", s.cancelBatch)
	}
	if store.Encrypted() {
		mux.HandleFunc("GET /v1/continuations/{id}", s.recoverContinuation)
		mux.HandleFunc("GET /v1/continuation-submissions/{submission}", s.recoverContinuation)
	}
	mux.HandleFunc("GET /v1/responses/{id}", s.getResponse)
	mux.HandleFunc("DELETE /v1/responses/{id}", s.deleteResponse)
	mux.HandleFunc("POST /v1/responses/{id}/cancel", s.cancelResponse)
	mux.HandleFunc("GET /v1/responses/{id}/input_items", s.responseInputItems)
}

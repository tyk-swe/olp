// Package geministate serves native Gemini files, resumable uploads and batches.
package geministate

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
)

type surface struct {
	kernel *kernel.Server
	store  *resources.Store
	spool  *staging.Spool
}

func Actions(k *kernel.Server, store *resources.Store, spool *staging.Spool) kernel.GeminiActions {
	return kernel.GeminiActions{"batchGenerateContent": surface{kernel: k, store: store, spool: spool}.createBatch}
}

func Register(k *kernel.Server, mux *http.ServeMux, store *resources.Store, spool *staging.Spool) {
	if store == nil || spool == nil {
		return
	}
	s := surface{kernel: k, store: store, spool: spool}
	mux.HandleFunc("OPTIONS /gemini/upload/", k.Preflight)
	mux.HandleFunc("OPTIONS /gemini/download/", k.Preflight)
	mux.HandleFunc("POST /gemini/upload/v1beta/files", s.startUpload)
	mux.HandleFunc("POST /gemini/upload/{session}", s.continueUpload)
	mux.HandleFunc("GET /gemini/v1beta/files", s.listFiles)
	mux.HandleFunc("GET /gemini/v1beta/files/{name}", s.getFile)
	mux.HandleFunc("DELETE /gemini/v1beta/files/{name}", s.deleteFile)
	mux.HandleFunc("GET /gemini/download/v1beta/files/{action}", s.downloadFile)
	for _, version := range []string{"v1", "v1beta"} {
		prefix := "/gemini/" + version + "/batches"
		mux.HandleFunc("GET "+prefix, s.listBatches)
		mux.HandleFunc("GET "+prefix+"/{name}", s.getBatch)
		mux.HandleFunc("POST "+prefix+"/{action}", s.cancelBatch)
		mux.HandleFunc("DELETE "+prefix+"/{name}", s.deleteBatch)
	}
}

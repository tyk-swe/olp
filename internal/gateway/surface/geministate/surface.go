// Package geministate serves native Gemini files, resumable uploads and batches.
package geministate

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type surface struct{ kernel *kernel.Server }

func Actions(k *kernel.Server) kernel.GeminiActions {
	return kernel.GeminiActions{"batchGenerateContent": surface{k}.geminiCreateBatch}
}

func Register(k *kernel.Server, mux *http.ServeMux) {
	if k.Deps.Resources == nil || k.Deps.Staging == nil {
		return
	}
	s := surface{k}
	mux.HandleFunc("OPTIONS /gemini/upload/", k.Preflight)
	mux.HandleFunc("OPTIONS /gemini/download/", k.Preflight)
	mux.HandleFunc("POST /gemini/upload/v1beta/files", s.startUpload)
	mux.HandleFunc("POST /gemini/upload/{session}", s.continueUpload)
	mux.HandleFunc("GET /gemini/v1beta/files", s.geminiListFiles)
	mux.HandleFunc("GET /gemini/v1beta/files/{name}", s.geminiGetFile)
	mux.HandleFunc("DELETE /gemini/v1beta/files/{name}", s.geminiDeleteFile)
	mux.HandleFunc("GET /gemini/download/v1beta/files/{action}", s.geminiDownloadFile)
	for _, version := range []string{"v1", "v1beta"} {
		prefix := "/gemini/" + version + "/batches"
		mux.HandleFunc("GET "+prefix, s.geminiListBatches)
		mux.HandleFunc("GET "+prefix+"/{name}", s.geminiGetBatch)
		mux.HandleFunc("POST "+prefix+"/{action}", s.geminiBatchCancel)
		mux.HandleFunc("DELETE "+prefix+"/{name}", s.geminiDeleteBatch)
	}
}

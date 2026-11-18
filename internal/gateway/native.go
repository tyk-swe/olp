package gateway

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/models"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) registerNative(mux *http.ServeMux, catalog *models.Handler) {
	mux.HandleFunc("POST /native/{dialect}/models/{model}", func(w http.ResponseWriter, r *http.Request) {
		// A native generation dialect is generation on the native surface;
		// every other native dialect is a registered unary operation.
		if family, ok := protocols.NativeGenerationFamily(r.PathValue("dialect")); ok {
			s.inferenceOperation(family, "")(w, r)
			return
		}
		s.inferenceOperation("", r.PathValue("dialect"))(w, r)
	})
	mux.HandleFunc("GET /native/{dialect}/sessions/{route}", s.nativeDuplex)
	for _, method := range routing.PassthroughMethods() {
		mux.HandleFunc(method+" /native/{dialect}/routes/{route}/{path...}", s.passthrough)
	}
	mux.HandleFunc("OPTIONS /native/", s.preflight)
	mux.HandleFunc("POST /v1/completions", s.inference(wire.Completions))
	mux.HandleFunc("POST /v1/decisions", s.inference(wire.Decisions))
	mux.HandleFunc("POST /gemini/v1beta/decisions", s.inference(wire.GeminiDecisions))
	mux.HandleFunc("POST /v1/ocr", s.inference(wire.OCR))
	mux.HandleFunc("POST /v1/search", s.inference(wire.Search))
	mux.HandleFunc("POST /v1/systemone", s.inference(wire.SystemOne))
	mux.HandleFunc("POST /v1/responses/input_tokens", s.inference(wire.InputTokens))
	mux.HandleFunc("POST /v1/embeddings", s.inference(wire.Embeddings))
	mux.HandleFunc("POST /v1/rerank", s.inference(wire.Rerank))
	mux.HandleFunc("POST /v1/moderations", s.inference(wire.Moderation))
	mux.HandleFunc("POST /anthropic/v1/messages", s.inference(wire.Anthropic))
	mux.HandleFunc("POST /anthropic/v1/messages/count_tokens", s.inference(wire.AnthropicCount))
	for _, prefix := range []string{"/anthropic/v1", "/gemini/v1", "/gemini/v1beta"} {
		mux.HandleFunc("GET "+prefix+"/models", catalog.Native)
		mux.HandleFunc("GET "+prefix+"/models/{model}", catalog.Native)
		mux.HandleFunc("OPTIONS "+prefix+"/", s.preflight)
		mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			s.begin(w, r)
			fault.WriteSurfaceError(w, fault.NotFoundError("not_found", "Unknown endpoint."), fault.RequestSurface(r))
		})
		if strings.HasPrefix(prefix, "/gemini/") {
			mux.HandleFunc("POST "+prefix+"/models/{action}", func(w http.ResponseWriter, r *http.Request) {
				model, action, ok := strings.Cut(r.PathValue("action"), ":")
				if !ok {
					s.begin(w, r)
					fault.WriteSurfaceError(w, fault.NotFoundError("not_found", "Unknown Gemini operation."), "gemini")
					return
				}
				var family wire.Format
				switch action {
				case "generateContent":
					family = wire.Gemini
				case "streamGenerateContent":
					family = wire.GeminiStream
				case "countTokens":
					family = wire.GeminiCount
				case "embedContent":
					family = wire.GeminiEmbeddings
				case "batchEmbedContents":
					family = wire.GeminiEmbeddingsBatch
				case "batchGenerateContent":
					r.SetPathValue("model", model)
					s.geminiCreateBatch(w, r)
					return
				default:
					s.begin(w, r)
					fault.WriteSurfaceError(w, fault.NotFoundError("not_found", "Unknown Gemini operation."), "gemini")
					return
				}
				r.SetPathValue("model", model)
				s.inference(family)(w, r)
			})
		}
	}
}

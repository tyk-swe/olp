package gateway

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/surface"
)

// requestSurface names the inference API a gateway request speaks.
func requestSurface(r *http.Request) string {
	if classified := surface.Of(r.URL.Path); classified.Inference {
		return classified.Name
	}
	return "openai"
}
func (s *Server) registerNative(mux *http.ServeMux) {
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
	for _, method := range routing.PassthroughMethods {
		mux.HandleFunc(method+" /native/{dialect}/routes/{route}/{path...}", s.passthrough)
	}
	mux.HandleFunc("OPTIONS /native/", s.preflight)
	mux.HandleFunc("POST /v1/completions", s.inference(wire.Completions))
	mux.HandleFunc("POST /v1/decisions", s.inference(wire.Decisions))
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
		mux.HandleFunc("GET "+prefix+"/models", s.nativeModels)
		mux.HandleFunc("GET "+prefix+"/models/{model}", s.nativeModels)
		mux.HandleFunc("OPTIONS "+prefix+"/", s.preflight)
		mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			s.begin(w, r)
			writeSurfaceError(w, notFoundError("not_found", "Unknown endpoint."), requestSurface(r))
		})
		if strings.HasPrefix(prefix, "/gemini/") {
			mux.HandleFunc("POST "+prefix+"/models/{action}", func(w http.ResponseWriter, r *http.Request) {
				model, action, ok := strings.Cut(r.PathValue("action"), ":")
				if !ok {
					s.begin(w, r)
					writeSurfaceError(w, notFoundError("not_found", "Unknown Gemini operation."), "gemini")
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
					writeSurfaceError(w, notFoundError("not_found", "Unknown Gemini operation."), "gemini")
					return
				}
				r.SetPathValue("model", model)
				s.inference(family)(w, r)
			})
		}
	}
}
func (s *Server) nativeModel(ctx context.Context, release *runtime.Release, authority access.Authority, route routing.Route, surface string) map[string]any {
	capabilities := s.modelCapabilities(ctx, release, authority, route, surface)
	if surface == "anthropic" {
		return map[string]any{"id": route.Slug, "type": "model", "display_name": route.Slug, "created_at": route.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"), "capabilities": capabilities}
	}
	methods := []string{}
	if slices.Contains(route.Operations, "generation") {
		methods = append(methods, "generateContent", "streamGenerateContent")
	}
	if slices.Contains(route.Operations, "token_count") {
		methods = append(methods, "countTokens")
	}
	return map[string]any{"name": "models/" + route.Slug, "displayName": route.Slug, "supportedGenerationMethods": methods, "capabilities": capabilities}
}
func (s *Server) nativeModels(w http.ResponseWriter, r *http.Request) {
	req := s.begin(w, r)
	surface := requestSurface(r)
	authority, e := s.authenticate(r, "models_read")
	if e != nil {
		writeSurfaceError(w, e, surface)
		return
	}
	if slug := r.PathValue("model"); slug != "" {
		route, ok := req.release.Snapshot.Routes[slug]
		if !ok || !authority.Allows("models_read", slug, route.ProjectID, s.now()) {
			writeSurfaceError(w, modelNotFound(slug), surface)
			return
		}
		writeJSON(w, s.nativeModel(r.Context(), req.release, authority, route, surface))
		return
	}
	rows := []map[string]any{}
	for _, slug := range slices.Sorted(mapsKeys(req.release.Snapshot.Routes)) {
		if authority.Allows("models_read", slug, req.release.Snapshot.Routes[slug].ProjectID, s.now()) {
			rows = append(rows, s.nativeModel(r.Context(), req.release, authority, req.release.Snapshot.Routes[slug], surface))
		}
	}
	if surface == "anthropic" {
		var first, last any
		if len(rows) > 0 {
			first = rows[0]["id"]
			last = rows[len(rows)-1]["id"]
		}
		writeJSON(w, map[string]any{"data": rows, "has_more": false, "first_id": first, "last_id": last})
	} else {
		writeJSON(w, map[string]any{"models": rows})
	}
}

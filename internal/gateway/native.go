package gateway

import (
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func requestSurface(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/anthropic/") {
		return "anthropic"
	}
	if strings.HasPrefix(r.URL.Path, "/gemini/") {
		return "gemini"
	}
	return "openai"
}
func (s *Server) registerNative(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/responses/input_tokens", s.inference(openai.FamilyInputTokens))
	mux.HandleFunc("POST /v1/embeddings", s.inference(openai.FamilyEmbeddings))
	mux.HandleFunc("POST /v1/rerank", s.inference(openai.FamilyRerank))
	mux.HandleFunc("POST /v1/moderations", s.inference(openai.FamilyModeration))
	mux.HandleFunc("POST /anthropic/v1/messages", s.inference(openai.FamilyAnthropic))
	mux.HandleFunc("POST /anthropic/v1/messages/count_tokens", s.inference(openai.FamilyAnthropicCount))
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
				var family openai.Family
				switch action {
				case "generateContent":
					family = openai.FamilyGemini
				case "streamGenerateContent":
					family = openai.FamilyGeminiStream
				case "countTokens":
					family = openai.FamilyGeminiCount
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
func nativeModel(snapshot *runtime.Snapshot, route runtime.Route, surface string) map[string]any {
	capabilities := runtime.EffectiveCapabilities(snapshot, route)
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
		writeJSON(w, nativeModel(req.release.Snapshot, route, surface))
		return
	}
	rows := []map[string]any{}
	for _, slug := range slices.Sorted(mapsKeys(req.release.Snapshot.Routes)) {
		if authority.Allows("models_read", slug, req.release.Snapshot.Routes[slug].ProjectID, s.now()) {
			rows = append(rows, nativeModel(req.release.Snapshot, req.release.Snapshot.Routes[slug], surface))
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

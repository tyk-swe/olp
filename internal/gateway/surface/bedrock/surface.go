package bedrock

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

type surface struct {
	kernel  *kernel.Server
	runtime kernel.Runtime
}

func ConverseHandler(k *kernel.Server, runtime kernel.Runtime) http.HandlerFunc {
	return surface{kernel: k, runtime: runtime}.converse
}

func ConverseStreamHandler(k *kernel.Server, runtime kernel.Runtime) http.HandlerFunc {
	return surface{kernel: k, runtime: runtime}.converseStream
}

func Register(k *kernel.Server, mux *http.ServeMux, runtime kernel.Runtime, store *resources.Store) {
	if store == nil {
		return
	}
	s := surface{kernel: k, runtime: runtime}
	mux.HandleFunc("POST /bedrock/model/{model}/converse", s.converse)
	mux.HandleFunc("POST /bedrock/model/{model}/converse-stream", s.converseStream)
	mux.HandleFunc("POST /bedrock/model/{model}/invoke", s.invoke)
	mux.HandleFunc("POST /bedrock/model/{model}/invoke-with-response-stream", s.invokeStream)
}

func qualified(p *routing.Provider, model, operation, mode string) bool {
	return p.Kind == "bedrock" &&
		p.Connector().Supports(operation, "bedrock", mode) &&
		p.Supports(model, operation, "bedrock", mode)
}

func (s surface) converse(w http.ResponseWriter, r *http.Request) {
	if s.preparedConverse(w, r) {
		return
	}
	s.serve(w, r, wire.Bedrock, "generation", "unary", "converse")
}

func (s surface) converseStream(w http.ResponseWriter, r *http.Request) {
	if s.preparedConverse(w, r) {
		return
	}
	s.serve(w, r, wire.Bedrock, "generation", "streaming", "converse-stream")
}

func (s surface) invoke(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, wire.BedrockInvoke, "bedrock_invoke", "unary", "invoke")
}

func (s surface) invokeStream(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, wire.BedrockInvoke, "bedrock_invoke", "streaming", "invoke-with-response-stream")
}

// Strict and cache-enabled Converse routes use the shared prepared invocation
// and Attempt owner. Its native codec retains AWS event-stream framing.
func (s surface) preparedConverse(w http.ResponseWriter, r *http.Request) bool {
	release := s.runtime.Release()
	if release == nil {
		return false
	}
	route, ok := release.Snapshot().FindRoute(r.PathValue("model"))
	if !ok || !route.UsesPreparedBedrockConverse() {
		return false
	}
	r = r.Clone(r.Context())
	if token := strings.TrimSpace(r.Header.Get("X-OLP-API-Key")); token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	s.kernel.InferenceHandler(wire.Bedrock)(w, r)
	return true
}

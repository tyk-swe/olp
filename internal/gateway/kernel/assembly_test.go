package kernel_test

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/surface/bedrock"
)

func init() {
	kernel.RegisterGateway = func(k *kernel.Server, mux *http.ServeMux) { gateway.Register(k, mux, k.Deps) }
	kernel.BedrockConverse = func(k *kernel.Server) http.HandlerFunc { return bedrock.ConverseHandler(k, k.Deps.Runtime) }
	kernel.BedrockConverseStream = func(k *kernel.Server) http.HandlerFunc { return bedrock.ConverseStreamHandler(k, k.Deps.Runtime) }
}

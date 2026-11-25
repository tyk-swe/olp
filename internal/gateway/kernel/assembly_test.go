package kernel_test

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/surface/bedrock"
)

func init() {
	kernel.RegisterGateway = func(k *kernel.Server, mux *http.ServeMux) { gateway.Register(k, mux) }
	kernel.BedrockConverse = bedrock.ConverseHandler
	kernel.BedrockConverseStream = bedrock.ConverseStreamHandler
}

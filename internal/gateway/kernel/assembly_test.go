package kernel_test

import (
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/surface/bedrock"
)

func init() {
	kernel.RegisterGateway = gateway.Register
	kernel.BedrockConverse = bedrock.ConverseHandler
	kernel.BedrockConverseStream = bedrock.ConverseStreamHandler
}

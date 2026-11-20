package kernel_test

import (
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

func init() { kernel.RegisterGateway = gateway.Register }

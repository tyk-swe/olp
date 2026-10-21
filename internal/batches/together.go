package batches

import (
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

type TogetherReader struct {
	managedReader
}

func NewTogetherReader(auth *connectors.Auth, policy *egress.Policy) *TogetherReader {
	return &TogetherReader{managedReader{adapter: "together", auth: auth, egress: policy}}
}

package batches

import (
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

type MistralReader struct {
	managedReader
}

func NewMistralReader(auth *connectors.Auth, policy *egress.Policy) *MistralReader {
	return &MistralReader{managedReader{adapter: "mistral", auth: auth, egress: policy}}
}

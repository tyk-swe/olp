package routing

import (
	"encoding/json"
)

func withMetadata(s *Snapshot, providerID, model string, m ModelMetadata) {
	encoded, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	p := s.Providers[providerID]
	if p.Models == nil {
		p.Models = map[string]json.RawMessage{}
	}
	p.Models[model] = encoded
	s.Providers[providerID] = p
}

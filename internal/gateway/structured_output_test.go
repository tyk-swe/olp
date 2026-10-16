package gateway

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/routing"
)

const jsonSchemaField = `,"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}`

func declareParams(h *harness, model string, params []string) {
	h.t.Helper()
	snap := h.rt.release.Snapshot
	for id, p := range snap.Providers {
		if len(p.Capabilities) == 0 || p.Capabilities[0].Model != model {
			continue
		}
		metadata, err := json.Marshal(routing.ModelMetadata{SupportedParameters: &params})
		if err != nil {
			h.t.Fatal(err)
		}
		if p.Models == nil {
			p.Models = map[string]json.RawMessage{}
		}
		p.Models[model] = metadata
		snap.Providers[id] = p
	}
}

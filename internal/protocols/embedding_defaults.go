package protocols

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// ApplyEmbeddingDefaults owns the native positions of embedding controls.
// Gemini batch controls belong to each request, never an invented batch-global
// field. Vertex parameters and other objects replace atomically when omitted.
func ApplyEmbeddingDefaults(prepared oif.Prepared, wire openai.Family, defaults Object) (oif.Prepared, error) {
	if len(defaults) == 0 {
		return prepared, nil
	}
	if wire != openai.FamilyGeminiEmbeddings && wire != openai.FamilyGeminiEmbeddingsBatch && wire != openai.FamilyVertexEmbeddings && wire != openai.FamilyBedrockEmbeddings {
		return prepared, nil
	}
	body, err := object(prepared.Document().Bytes())
	if err != nil {
		return oif.Prepared{}, err
	}
	changes := []oif.Provenance{}
	apply := func(fields Object, prefix string) {
		for _, name := range slices.Sorted(maps.Keys(defaults)) {
			if _, present := fields[name]; present {
				continue
			}
			fields[name] = defaults[name]
			changes = append(changes, oif.Provenance{Pointer: prefix + "/" + strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1"), Origin: oif.ProviderDefault, Reason: "fill omitted native embedding control"})
		}
	}
	if wire == openai.FamilyGeminiEmbeddingsBatch {
		var requests []json.RawMessage
		if json.Unmarshal(body["requests"], &requests) != nil {
			return oif.Prepared{}, protocolError("invalid Gemini embedding batch")
		}
		for i, raw := range requests {
			fields, err := object(raw)
			if err != nil {
				return oif.Prepared{}, err
			}
			apply(fields, fmt.Sprintf("/requests/%d", i))
			requests[i], _ = json.Marshal(fields)
		}
		body["requests"], _ = json.Marshal(requests)
	} else {
		apply(body, "")
	}
	if len(changes) == 0 {
		return prepared, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return oif.Prepared{}, err
	}
	document, err := oif.ParseJSON(encoded, prepared.Document().Limits())
	if err != nil {
		return oif.Prepared{}, err
	}
	result, err := oif.PrepareDestination(prepared.Request(), prepared.Descriptor(), document, oif.ProviderDefault, "operation-owned native embedding defaults")
	if err != nil {
		return oif.Prepared{}, err
	}
	return result.WithProvenance(append(prepared.Provenance(), changes...)...), nil
}

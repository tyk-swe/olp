package protocols_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/vendors"
)

const bodyModel = "fixture-body"

var nativeBodies = map[wire.Format]string{
	wire.GeminiCount:           `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`,
	wire.BedrockCount:          `{"input":{"converse":{"messages":[{"role":"user","content":[{"text":"hello"}]}]}}}`,
	wire.BedrockEmbeddings:     `{"inputText":"first"}`,
	wire.BedrockRerank:         `{"queries":[{"type":"TEXT","textQuery":{"text":"weather"}}],"sources":[{"type":"INLINE","inlineDocumentSource":{"type":"TEXT","textDocument":{"text":"sunny"}}}]}`,
	wire.GeminiEmbeddings:      `{"content":{"parts":[{"text":"first"}]}}`,
	wire.GeminiEmbeddingsBatch: `{"requests":[{"content":{"parts":[{"text":"first"}]}}]}`,
	wire.VertexEmbeddings:      `{"instances":[{"content":"first"}]}`,
}

func nativeBody(format wire.Format) string {
	if body, ok := nativeBodies[format]; ok {
		return body
	}
	index := slices.IndexFunc(requestFixtures, func(fixture requestFixture) bool { return fixture.format == format })
	if index < 0 {
		return `{"input":"probe"}`
	}
	return requestFixtures[index].body
}

func modelBodies(t *testing.T, format wire.Format) map[string][]byte {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(nativeBody(format)), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "model")
	unnamed, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	fields["model"] = json.RawMessage(`"` + bodyModel + `"`)
	named, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"body names a model": named, "body names no model": unnamed}
}

func member(document oif.Document, pointer string) any {
	value, found := document.Lookup(pointer)
	if !found {
		return nil
	}
	return json.RawMessage(value.Raw())
}

func modelOutcome(prepared oif.Prepared, err error) any {
	if err != nil {
		return map[string]any{"error": classify(err)}
	}
	out := map[string]any{"model": member(prepared.Document(), "/model")}
	if nested := member(prepared.Document(), "/generateContentRequest/model"); nested != nil {
		out["nested_model"] = nested
	}
	return out
}

func servingVendor(operation string) string {
	for _, contract := range vendors.All() {
		if _, _, reviewed := vendors.Shape(contract.ID, operation); reviewed && vendors.Serves(contract.ID, operation) {
			return contract.ID
		}
	}
	return ""
}

func TestModelInBodyRules(t *testing.T) {
	t.Parallel()
	rerankVendor := servingVendor(wire.Rerank.Operation())
	cases := []goldenCase{}
	for _, format := range formats(t) {
		vendor := ""
		if format == wire.Rerank {
			vendor = rerankVendor
		}
		cases = append(cases, goldenCase{string(format), func(t *testing.T) any {
			identity, native := map[string]any{}, map[string]any{}
			for name, body := range modelBodies(t, format) {
				request, err := requestOf(format, body)
				if err != nil {
					t.Fatal(err)
				}
				identity[name] = modelOutcome(protocols.PrepareIdentity(request, upstreamModel))
				prepared, _, err := protocols.PrepareTarget(request, format, "", vendor, upstreamModel, nil)
				native[name] = modelOutcome(prepared, err)
			}
			return map[string]any{
				"identity binding": collapsed("either body", identity),
				"native encoding":  collapsed("either body", native),
			}
		}})
	}
	checkGolden(t, "testdata/model_in_body.golden.json", cases)
}

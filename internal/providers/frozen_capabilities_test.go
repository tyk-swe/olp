package providers

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// Compare the public certification matrix with the independent frozen contract.
// Supports alone is broader: Azure and generic OpenAI transports cannot certify
// media, even though they share the OpenAI wire implementation.
func TestFrozenCertificationMatrix(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/reference-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var reference struct {
		Inference []struct{ Provider, Operation, Surface, Transport string }
	}
	if err := json.Unmarshal(data, &reference); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(reference.Inference))
	for _, tuple := range reference.Inference {
		want = append(want, strings.Join([]string{tuple.Provider, tuple.Operation, tuple.Surface, tuple.Transport}, "/"))
	}
	// Additive native audio translation is qualified by the public strict-route
	// and pinned OpenAI SDK tests; the independent frozen inventory is unchanged.
	want = append(want, "OpenAi/Translation/OpenAi/Unary")
	names := map[string]string{
		KindOpenAI: "OpenAi", KindOpenAICompatible: "OpenAiCompatible",
		KindAnthropic: "Anthropic", KindGemini: "Gemini", KindAzure: "AzureOpenAi",
		KindVertex: "VertexAi", KindBedrock: "Bedrock",
	}
	pascal := func(value string) string {
		parts := strings.Split(value, "_")
		for i, part := range parts {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
		return strings.Join(parts, "")
	}
	var got []string
	for _, kind := range kinds {
		for _, tuple := range capabilitiesFor(kind.Kind, defaultVendor(kind.Kind)) {
			got = append(got, strings.Join([]string{names[kind.Kind], pascal(tuple.Operation), names[tuple.Surface], pascal(tuple.Mode)}, "/"))
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		for _, tuple := range want {
			if !slices.Contains(got, tuple) {
				t.Errorf("lost frozen capability: %s", tuple)
			}
		}
		for _, tuple := range got {
			if !slices.Contains(want, tuple) {
				t.Errorf("capability requires an explicit compatibility review: %s", tuple)
			}
		}
		t.Fatalf("certifiable matrix differs from frozen reference: got %d tuples, want %d", len(got), len(want))
	}
}

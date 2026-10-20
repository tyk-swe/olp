package providers

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files from the current behavior")

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed; review the difference as a change of the onboarding catalogue and rerun with -update", path)
	}
}

func indented(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

func TestProviderCatalogueGoldens(t *testing.T) {
	golden(t, "kinds.golden", indented(t, map[string]any{"items": kinds}))
	golden(t, "vendors.golden", indented(t, vendorCatalogue))
	var lines []string
	kindNames := []string{"openai", "openai_compatible", "anthropic", "gemini", "azure_openai", "vertex_ai", "bedrock", KindSageMaker, KindWatsonx, KindPlugin}
	vendorIDs := []string{"", "unreviewed", "openai", "openai_compatible", "anthropic", "google", "google-vertex", "amazon-bedrock", "amazon-sagemaker", "ibm-watsonx", "azure",
		"groq", "mistral", "openrouter", "together", "vllm", "deepseek", "fireworks", "deepinfra", "huggingface", "cohere", "cohere-native-v2", "voyage"}
	for _, kind := range kindNames {
		for _, vendor := range vendorIDs {
			capabilities := capabilitiesFor(kind, vendor)
			if len(capabilities) == 0 {
				continue
			}
			tuples := make([]string, 0, len(capabilities))
			for _, capability := range capabilities {
				tuples = append(tuples, capability.Operation+"/"+capability.Surface+"/"+capability.Mode)
			}
			lines = append(lines, kind+" "+vendor+": "+strings.Join(tuples, " "))
		}
	}
	golden(t, "certifiable.golden", []byte(strings.Join(lines, "\n")+"\n"))
}

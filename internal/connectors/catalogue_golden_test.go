package connectors

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
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
		t.Fatalf("%s changed; review the difference as a change of the reviewed connector matrix and rerun with -update", path)
	}
}

// goldenVendors are the vendor identifiers the matrix is pinned for: every
// reviewed vendor, the kinds' own identifiers, and one nobody reviewed.
var goldenVendors = []string{"", "unreviewed", "openai", "openai_compatible", "anthropic", "google", "google-vertex", "amazon-bedrock", "amazon-sagemaker", "ibm-watsonx", "azure",
	"groq", "mistral", "openrouter", "together", "vllm", "deepseek", "fireworks", "deepinfra", "huggingface", "cohere", "cohere-native-v2", "voyage"}

var goldenKinds = []string{"openai", "openai_compatible", "anthropic", "gemini", "azure_openai", "vertex_ai", "bedrock", KindSageMaker, KindWatsonx, KindPlugin}

var goldenOperations = []string{"generation", "token_count", "embeddings", "moderation", "rerank", "batch", "realtime", "bedrock_invoke",
	"image_generation", "image_edit", "image_variation", "speech", "transcription", "translation",
	"video_create", "video_list", "video_get", "video_content", "video_delete"}

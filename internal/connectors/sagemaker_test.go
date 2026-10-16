package connectors

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/egress"
)

func sagemaker() Config {
	return Config{Kind: KindSageMaker, AuthMode: "static", CloudRegion: "us-west-2", VendorID: "amazon-sagemaker",
		Endpoint: DefaultEndpoint(KindSageMaker, "us-west-2", "")}
}

// TestSageMakerAddressesEndpointsAndComponents covers the OpenAI-compatible
// path: the URL routes to the endpoint, or to an inference component on it.
func TestSageMakerAddressesEndpointsAndComponents(t *testing.T) {
	c := sagemaker()
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		"qwen3-4b":            "https://runtime.sagemaker.us-west-2.amazonaws.com/endpoints/qwen3-4b/openai/v1/chat/completions",
		"shared-ep/qwen-ic-1": "https://runtime.sagemaker.us-west-2.amazonaws.com/endpoints/shared-ep/inference-components/qwen-ic-1/openai/v1/chat/completions",
	} {
		for _, stream := range []bool{false, true} {
			if got, err := c.URL(wire.Chat, model, stream); err != nil || got != want {
				t.Fatalf("URL(%s, stream %v) = %s, %v", model, stream, got, err)
			}
		}
	}
	if _, err := c.URL(wire.Embeddings, "qwen3-4b", false); err == nil {
		t.Fatal("SageMaker addressed an embeddings call")
	}
	for model, valid := range map[string]bool{
		"a": true, "my--endpoint-2": true, "ep/ic": true, strings.Repeat("e", 63): true,
		strings.Repeat("e", 64): false, "-ep": false, "ep-": false, "ep/ic/x": false, "ep/": false, "ep.v1": false, "../ep": false,
	} {
		if ModelValid(KindSageMaker, model) != valid || c.ValidModel(model) != valid {
			t.Fatalf("ValidModel(%q) = %v", model, !valid)
		}
	}
	if !c.Supports("generation", "openai", "streaming") || c.Supports("generation", "anthropic", "unary") || c.Supports("embeddings", "openai", "unary") || c.Supports("token_count", "openai", "unary") {
		t.Fatal("SageMaker serves OpenAI chat generation only")
	}
	if china := DefaultEndpoint(KindSageMaker, "cn-north-1", ""); china != "https://runtime.sagemaker.cn-north-1.amazonaws.com.cn" {
		t.Fatalf("China endpoint = %s", china)
	}
	for name, mutate := range map[string]func(*Config){
		"path":       func(c *Config) { c.Endpoint += "/endpoints/x" },
		"no region":  func(c *Config) { c.CloudRegion = "" },
		"deployment": func(c *Config) { c.Deployment = "x" },
	} {
		c := sagemaker()
		mutate(&c)
		if err := c.Validate(&egress.Policy{}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

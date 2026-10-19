//go:build liveproviders

package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestLiveProviderNativeGeneration(t *testing.T) {
	provider := os.Getenv("OLP_LIVE_PROVIDER")
	if provider == "" {
		t.Skip("set OLP_LIVE_PROVIDER; this test incurs provider charges")
	}
	require := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("%s is required", name)
		}
		return v
	}
	c := Config{Kind: provider, AuthMode: "api_key"}
	encodedWire := wire.Chat
	model := os.Getenv("OLP_LIVE_MODEL")
	var secret string
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Reply with the word hello."}}, "max_tokens": 16}
	switch provider {
	case "openai":
		secret = require("OLP_LIVE_OPENAI_API_KEY")
		if model == "" {
			model = "gpt-4o-mini"
		}
	case "anthropic":
		secret = require("OLP_LIVE_ANTHROPIC_API_KEY")
		encodedWire = wire.Anthropic
		if model == "" {
			model = "claude-sonnet-4-6"
		}
	case "gemini":
		secret = require("OLP_LIVE_GEMINI_API_KEY")
		encodedWire = wire.Gemini
		if model == "" {
			model = "gemini-2.5-flash"
		}
	case "azure_openai":
		secret = require("OLP_AZURE_OPENAI_LIVE_API_KEY")
		c.Endpoint = require("OLP_AZURE_OPENAI_LIVE_ENDPOINT")
		c.Deployment = require("OLP_AZURE_OPENAI_LIVE_DEPLOYMENT")
		c.APIVersion = require("OLP_AZURE_OPENAI_LIVE_API_VERSION")
		model = c.Deployment
	case "vertex":
		c.Kind = "vertex_ai"
		c.AuthMode = "adc"
		c.CloudProject = require("OLP_VERTEX_LIVE_PROJECT")
		c.CloudRegion = require("OLP_VERTEX_LIVE_LOCATION")
		model = require("OLP_VERTEX_LIVE_MODEL")
		encodedWire = wire.Gemini
	case "bedrock":
		c.AuthMode = "default_chain"
		c.CloudRegion = require("OLP_BEDROCK_LIVE_REGION")
		model = require("OLP_BEDROCK_LIVE_MODEL")
		encodedWire = wire.Format("bedrock")
	case "watsonx":
		c.AuthMode = "ibm_iam"
		secret = require("OLP_WATSONX_LIVE_API_KEY")
		c.CloudRegion = require("OLP_WATSONX_LIVE_REGION")
		c.CloudProject = require("OLP_WATSONX_LIVE_PROJECT")
		if model == "" {
			model = "ibm/granite-3-8b-instruct"
		}
	case "sagemaker":
		c.AuthMode = "default_chain"
		c.CloudRegion = require("OLP_SAGEMAKER_LIVE_REGION")
		// The endpoint, or endpoint/inference-component.
		model = require("OLP_SAGEMAKER_LIVE_MODEL")
	case "vertex-openai":
		c.Kind, c.AuthMode, c.ProfileID, c.ProfileRevision = "vertex_ai", "adc", "vertex-openai", ProfileRevision
		c.CloudProject = require("OLP_VERTEX_LIVE_PROJECT")
		c.CloudRegion = require("OLP_VERTEX_LIVE_LOCATION")
		c.Endpoint = DefaultProfileEndpoint(c.Kind, c.ProfileID, c.CloudRegion, c.CloudProject)
		if model == "" {
			model = "google/gemini-2.5-flash"
		}
	case "mistral-fim":
		c.Kind, c.ProfileID, c.ProfileRevision, c.VendorID = "openai_compatible", "mistral-fim", ProfileRevision, "mistral"
		c.Endpoint = "https://api.mistral.ai/v1"
		secret = require("OLP_LIVE_MISTRAL_API_KEY")
		encodedWire = wire.MistralFIM
		if model == "" {
			model = "codestral-latest"
		}
	case "cohere-chat":
		c.Kind, c.ProfileID, c.ProfileRevision, c.VendorID = "openai_compatible", "cohere-v2", ProfileRevision, "cohere-native-v2"
		c.Endpoint = "https://api.cohere.ai/v2"
		secret = require("OLP_LIVE_COHERE_API_KEY")
		encodedWire = wire.CohereChat
		if model == "" {
			model = "command-a-03-2025"
		}
	case "bedrock-rerank":
		c.Kind, c.AuthMode, c.VendorID = "bedrock", "default_chain", "amazon-bedrock"
		c.CloudRegion = require("OLP_BEDROCK_LIVE_REGION")
		encodedWire = wire.BedrockRerank
		if model == "" {
			model = "cohere.rerank-v3-5:0"
		}
	case "bedrock-image":
		c.Kind, c.AuthMode, c.VendorID = "bedrock", "default_chain", "amazon-bedrock"
		c.CloudRegion = require("OLP_BEDROCK_LIVE_REGION")
		if model == "" {
			model = "stability.stable-image-core-v1:1"
		}
	default:
		t.Fatalf("unsupported provider %q", provider)
	}
	body["model"] = model
	switch provider {
	case "mistral-fim":
		body = map[string]any{"model": model, "prompt": "def add(a, b):\n    return ", "suffix": "\n", "max_tokens": 16}
	case "bedrock-rerank":
		body = map[string]any{"queries": []any{map[string]any{"type": "TEXT", "textQuery": map[string]string{"text": "What is Amazon Bedrock?"}}},
			"sources":                []any{map[string]any{"type": "INLINE", "inlineDocumentSource": map[string]any{"type": "TEXT", "textDocument": map[string]string{"text": "Amazon Bedrock is a managed service for foundation models."}}}},
			"rerankingConfiguration": map[string]any{"type": "BEDROCK_RERANKING_MODEL", "bedrockRerankingConfiguration": map[string]any{"modelConfiguration": map[string]string{"modelArn": c.BedrockModelARN(model)}}}}
	case "bedrock-image":
		body = map[string]any{"prompt": "A small blue square."}
	}
	if encodedWire == wire.Gemini {
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "Reply with the word hello."}}}}, "generationConfig": map[string]int{"maxOutputTokens": 32}}
	}
	if provider == "bedrock" {
		body = map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]string{"text": "Reply with the word hello."}}}}, "inferenceConfig": map[string]int{"maxTokens": 16}}
	}
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint(c.Kind, c.CloudRegion, c.CloudProject)
	}
	policy := &egress.Policy{}
	if err := c.Validate(policy); err != nil {
		t.Fatal(err)
	}
	endpoint, err := c.URL(encodedWire, model, false)
	if provider == "bedrock-image" {
		endpoint, err = c.MediaURL("model/"+url.PathEscape(model)+"/invoke", model, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	encoded = c.WrapRequest(encoded, model)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal("invalid native request")
	}
	req.Header.Set("Content-Type", "application/json")
	if _, err = NewAuth(policy).Apply(ctx, req, c, []byte(secret), encoded); err != nil {
		t.Fatal(err)
	}
	resp, err := policy.Client(30 * time.Second).Do(req)
	if err != nil {
		t.Fatal("live provider transport failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !json.Valid(b) {
		t.Fatalf("native generation status=%d valid_json=%v", resp.StatusCode, json.Valid(b))
	}
}

// TestLiveProviderOperation certifies a reviewed OCR or search dialect against
// the live provider it binds to. It is opt-in only: it sends a probe request
// and incurs provider charges.
func TestLiveProviderOperation(t *testing.T) {
	dialect := os.Getenv("OLP_LIVE_OPERATION_DIALECT")
	if dialect == "" {
		t.Skip("set OLP_LIVE_OPERATION_DIALECT; this test incurs provider charges")
	}
	codec, ok := operationregistry.Lookup(dialect)
	if !ok {
		t.Fatalf("unknown operation dialect %q", dialect)
	}
	require := func(name string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		t.Fatalf("%s is required", name)
		return ""
	}
	c := Config{Kind: "openai_compatible", AuthMode: "api_key", ProfileID: dialect, ProfileRevision: ProfileRevision}
	var secret string
	switch dialect {
	case "mistral-ocr":
		c.VendorID, c.Endpoint = "mistral", "https://api.mistral.ai/v1"
		secret = require("OLP_LIVE_MISTRAL_API_KEY")
	case "perplexity-search":
		c.VendorID, c.Endpoint = "perplexity", "https://api.perplexity.ai"
		secret = require("OLP_LIVE_PERPLEXITY_API_KEY")
	case "tavily-search":
		c.VendorID, c.Endpoint = "tavily", "https://api.tavily.com"
		secret = require("OLP_LIVE_TAVILY_API_KEY")
	case "exa-search":
		c.VendorID, c.Endpoint = "exa", "https://api.exa.ai"
		secret = require("OLP_LIVE_EXA_API_KEY")
	case "parallel-search":
		c.VendorID, c.Endpoint = "parallel", "https://api.parallel.ai/v1"
		secret = require("OLP_LIVE_PARALLEL_API_KEY")
	default:
		t.Fatalf("dialect %q has no live qualification wiring", dialect)
	}
	if override := os.Getenv("OLP_LIVE_ENDPOINT"); override != "" {
		c.Endpoint = override
	}
	model := os.Getenv("OLP_LIVE_MODEL")
	if model == "" {
		model = "olp-live-probe"
	}
	body := codec.Probe(model)
	endpoint, err := c.OperationURL(codec, model)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if _, err := NewAuth(&egress.Policy{}).Apply(t.Context(), req, c, []byte(secret), body); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned %d: %s", endpoint, resp.StatusCode, raw)
	}
	in, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := oif.NewRequest(oif.Descriptor{Operation: codec.Operation, Dialect: codec.Identity}, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := oif.ParseJSON(raw, oif.Limits{})
	if err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	result, err := oif.NewResult(oif.Descriptor{Operation: codec.Operation, Dialect: codec.Identity}, out, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Result(request, result); err != nil {
		t.Fatalf("live result violates the %s contract: %v", dialect, err)
	}
}

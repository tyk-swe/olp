//go:build liveproviders

package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
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
	wire := openai.FamilyChat
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
		wire = openai.FamilyAnthropic
		if model == "" {
			model = "claude-sonnet-4-6"
		}
	case "gemini":
		secret = require("OLP_LIVE_GEMINI_API_KEY")
		wire = openai.FamilyGemini
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
		wire = openai.FamilyGemini
	case "bedrock":
		c.AuthMode = "default_chain"
		c.CloudRegion = require("OLP_BEDROCK_LIVE_REGION")
		model = require("OLP_BEDROCK_LIVE_MODEL")
		wire = openai.Family("bedrock")
	default:
		t.Fatalf("unsupported provider %q", provider)
	}
	body["model"] = model
	if wire == openai.FamilyGemini {
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
	endpoint, err := c.URL(wire, model, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
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

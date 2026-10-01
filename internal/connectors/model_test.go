package connectors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestBoundModelResourceNameIsNormalizedInURL(t *testing.T) {
	gemini := Config{Kind: "gemini", Endpoint: "https://generativelanguage.googleapis.com/v1beta", Bindings: map[string]Binding{"g": {Model: "models/gemini-2.5-pro"}}}
	got, err := gemini.URL(openai.FamilyGemini, "g", false)
	if want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent"; err != nil || got != want {
		t.Fatalf("binding URL = %q, %v; want %q", got, err, want)
	}
	if m := gemini.Model("g"); m != "gemini-2.5-pro" {
		t.Fatalf("binding Model = %q", m)
	}
	deployment := Config{Kind: "gemini", Endpoint: "https://generativelanguage.googleapis.com/v1beta", Models: map[string]json.RawMessage{"g": json.RawMessage(`{"deployment":"models/gemini-2.5-flash"}`)}}
	if m := deployment.Model("g"); m != "gemini-2.5-flash" {
		t.Fatalf("metadata deployment Model = %q", m)
	}
	vertex := Config{Kind: "vertex_ai", AuthMode: "service_account", CloudRegion: "global", CloudProject: "project", Bindings: map[string]Binding{"g": {Model: "models/gemini-2.5-pro"}}}
	vertex.Endpoint = DefaultEndpoint(vertex.Kind, vertex.CloudRegion, vertex.CloudProject)
	got, err = vertex.URL(openai.FamilyGeminiCount, "g", false)
	if err != nil || strings.Contains(got, "%2F") || !strings.HasSuffix(got, "/models/gemini-2.5-pro:countTokens") {
		t.Fatalf("vertex count URL = %q, %v", got, err)
	}
}

func TestModelResourcePrefixIsScopedToConnectorKind(t *testing.T) {
	for _, kind := range []string{"openai", "openai_compatible", "anthropic", "plugin", "bedrock", "gemini", "vertex_ai", "azure_openai"} {
		t.Run(kind, func(t *testing.T) {
			const upstream = "models/acme-v1"
			want := upstream
			if kind == "gemini" || kind == "vertex_ai" || kind == "azure_openai" {
				want = "acme-v1"
			}
			for _, config := range []Config{
				{Kind: kind},
				{Kind: kind, Bindings: map[string]Binding{upstream: {Model: upstream}}},
				{Kind: kind, Bindings: map[string]Binding{upstream: {Deployment: upstream}}},
				{Kind: kind, Models: map[string]json.RawMessage{upstream: json.RawMessage(`{"deployment":"models/acme-v1"}`)}},
			} {
				if got := config.Model(upstream); got != want {
					t.Errorf("Model = %q, want %q for %+v", got, want, config)
				}
			}
		})
	}
}

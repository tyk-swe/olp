package codeclient

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestResearchConfigurationUsesOLPKeyAndIndependentAccount(t *testing.T) {
	for _, test := range []struct {
		client  string
		model   string
		adapter codemode.Adapter
	}{
		{"codex", "gpt-5.5", codemode.AdapterCodex},
		{"opencode", "gpt-5.5", codemode.AdapterOpenCodeGo},
		{"claude-code", "minimax-m3", codemode.AdapterOpenCodeGo},
	} {
		t.Run(test.client, func(t *testing.T) {
			for _, platform := range []string{"linux", "macos", "windows"} {
				t.Run(platform, func(t *testing.T) {
					route := codemode.Route{Slug: "gpt-only", Enabled: true, RevisionID: "published", Models: []string{test.model}, Tools: &codemode.ResearchTools{AccountID: "11111111-1111-4111-8111-111111111111", Services: []string{"search", "reader", "zread"}}}
					offers := Offers{{Adapter: test.adapter, Models: route.Models}}
					config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example/prefix", Client: test.client, Platform: platform})
					if err != nil {
						t.Fatal(err)
					}
					text := config.Configuration
					if test.client == "claude-code" {
						format := map[string]string{"linux": "shell", "macos": "shell", "windows": "powershell"}[platform]
						if config.Format != format || len(config.AdditionalFiles) != 1 || config.AdditionalFiles[0].File != ".mcp.json" || !strings.Contains(text, "ANTHROPIC_AUTH_TOKEN") {
							t.Fatalf("companion setup: %+v", config)
						}
						text = config.AdditionalFiles[0].Configuration
					}
					if test.client != "codex" && !json.Valid([]byte(text)) {
						t.Fatalf("invalid JSON: %s", text)
					}
					for _, service := range route.Tools.Services {
						if !strings.Contains(text, "https://olp.example/prefix/code/gpt-only/mcp/"+service) || !strings.Contains(text, "olp_gpt-only_"+service) {
							t.Fatalf("missing service: %s", text)
						}
					}
					if !strings.Contains(text, "OLP_API_KEY") || strings.Contains(text, "api.z.ai") || strings.Contains(text, "glm-") || strings.Contains(text, `"tools"`) {
						t.Fatalf("unexpected client binding or tools override: %s", text)
					}
				})
			}
		})
	}
}

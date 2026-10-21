package codeclient

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestResearchConfigurationUsesOLPKeyAndIndependentAccount(t *testing.T) {
	route := codemode.Route{Slug: "gpt-only", Enabled: true, RevisionID: "published", Models: []string{"gpt-5.5"}, Tools: &codemode.ResearchTools{AccountID: "11111111-1111-4111-8111-111111111111", Services: []string{"search", "reader", "zread"}}}
	for _, client := range []string{"codex", "opencode", "claude-code"} {
		t.Run(client, func(t *testing.T) {
			offers := Offers{{Adapter: codemode.AdapterCodex, Models: route.Models}}
			if client == "opencode" {
				offers = Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: route.Models}}
			}
			if client == "claude-code" {
				route.Models = []string{"minimax-m3"}
				offers = Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: route.Models}}
			}
			config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example/prefix", Client: client})
			if err != nil {
				t.Fatal(err)
			}
			text := config.Configuration
			if client == "claude-code" {
				if config.Format != "shell" || len(config.AdditionalFiles) != 1 || config.AdditionalFiles[0].File != ".mcp.json" || !strings.Contains(text, "ANTHROPIC_AUTH_TOKEN") {
					t.Fatalf("companion setup: %+v", config)
				}
				text = config.AdditionalFiles[0].Configuration
			}
			if client != "codex" && !json.Valid([]byte(text)) {
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
}

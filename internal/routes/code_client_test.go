package routes

import (
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func TestCodexClientConfigurationPreservesNativeModelsAndUsesOnlyOLPKey(t *testing.T) {
	route := codemode.Route{Slug: "team-code", Enabled: true, RevisionID: "published", Models: []string{"gpt-5.4", "gpt-5.3-codex"}}
	config, err := CodexClientConfiguration(route, "https://gateway.example/olp/", "gpt-5.3-codex")
	if err != nil || config.BaseURL != "https://gateway.example/olp/code/team-code" || config.ClientVersion != "0.160.0" || len(config.NativeModels) != 2 {
		t.Fatalf("configuration: %+v %v", config, err)
	}
	if !reflect.DeepEqual(config.Adapters, []codemode.Adapter{codemode.AdapterCodex}) || config.Client != "codex" || config.Format != "toml" || *config.File != "$CODEX_HOME/config.toml" || config.Model != "gpt-5.3-codex" || config.PlanModel != nil || config.SmallModel != nil || !reflect.DeepEqual(config.SupportedClients, []string{"codex"}) {
		t.Fatalf("selection: %+v", config)
	}
	for _, want := range []string{`model = "gpt-5.3-codex"`, `env_key = "OLP_API_KEY"`, `requires_openai_auth = false`, `supports_websockets = true`, `name = "OpenAI"`, `http_headers = { "X-OLP-Code-Model" = "gpt-5.3-codex" }`} {
		if !strings.Contains(config.Configuration, want) {
			t.Fatalf("missing client setting %s", want)
		}
	}
	for _, forbidden := range []string{"refresh_token", "chatgpt.com", "OPENAI_API_KEY", "Authorization", "request_max_retries", "stream_max_retries"} {
		if strings.Contains(config.Configuration, forbidden) {
			t.Fatalf("configuration contains upstream credential material or first-party headers: %s", forbidden)
		}
	}
	config.NativeModels[0] = "changed"
	if route.Models[0] != "gpt-5.4" {
		t.Fatal("configuration mutated the published route")
	}
	if _, err := CodexClientConfiguration(route, "http://127.0.0.1:8080", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := CodexClientConfiguration(route, "https://gateway.example", "team-code"); err == nil {
		t.Fatal("route alias accepted as native model")
	}
	for _, invalid := range []string{"http://gateway.example", "https://key@gateway.example", "https://gateway.example?key=secret", "https://gateway.example#fragment", "/relative"} {
		if _, err := CodexClientConfiguration(route, invalid, ""); err == nil {
			t.Fatalf("accepted unsafe URL %s", invalid)
		}
	}
	route.RevisionID = ""
	if _, err := CodexClientConfiguration(route, "https://gateway.example", ""); err == nil {
		t.Fatal("unpublished draft got client configuration")
	}
}

func TestClaudeCodeConfigurationKeepsEveryModelOnTheRoute(t *testing.T) {
	route := codemode.Route{Slug: "team-glm", Enabled: true, RevisionID: "published", Models: []string{"glm-5.3", "glm-5.3-flash"}}
	config, err := ClientConfiguration(route, offer(codemode.AdapterZAICoding, route.Models...), ClientRequest{GatewayURL: "https://gateway.example/o'lp/", SmallModel: "glm-5.3-flash"})
	if err != nil || config.Client != "claude-code" || config.Format != "shell" || config.File != nil || config.ClientVersion != "2.1.286" || config.Model != "glm-5.3" || *config.PlanModel != "glm-5.3" || *config.SmallModel != "glm-5.3-flash" {
		t.Fatalf("configuration: %+v %v", config, err)
	}
	want := `# Claude Code 2.1.286 for OLP code-mode route team-glm (GLM Coding Plan).
# Source this file in a POSIX shell, such as bash or zsh, before starting claude.
# Set OLP_API_KEY to your OLP inference key first. No Anthropic login or vendor
# key belongs on this machine. To select other models, regenerate this file.
unset ANTHROPIC_API_KEY ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL='https://gateway.example/o%27lp/code/team-glm'
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_MODEL='glm-5.3'
export ANTHROPIC_DEFAULT_OPUS_MODEL='glm-5.3'
export ANTHROPIC_DEFAULT_SONNET_MODEL='glm-5.3'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='glm-5.3-flash'
export CLAUDE_CODE_SUBAGENT_MODEL='glm-5.3'
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
`
	if config.Configuration != want {
		t.Fatalf("configuration:\n%s", config.Configuration)
	}
	cmd := exec.Command("sh", "-c", `. /dev/stdin
printf '%s\n' "${ANTHROPIC_SMALL_FAST_MODEL-unset}" "$ANTHROPIC_DEFAULT_HAIKU_MODEL"
`)
	cmd.Stdin = strings.NewReader(config.Configuration)
	cmd.Env = []string{"OLP_API_KEY=olp-test", "ANTHROPIC_SMALL_FAST_MODEL=stale-model"}
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "unset\nglm-5.3-flash\n" {
		t.Fatalf("inherited background model override: %s (%v)", output, err)
	}
	if shellQuote(`it's`) != `'it'\''s'` {
		t.Fatal("shell quoting is unsafe")
	}
	if !reflect.DeepEqual(config.SupportedClients, []string{"claude-code", "opencode"}) || len(config.QualificationGaps) < 5 {
		t.Fatalf("clients %v gaps %v", config.SupportedClients, config.QualificationGaps)
	}
}

func TestOpenCodeConfigurationOverridesTheBuiltInProvider(t *testing.T) {
	route := codemode.Route{Slug: "team-go", Enabled: true, RevisionID: "published", Models: []string{"kimi-k3", "minimax-m3"}}
	config, err := ClientConfiguration(route, offer(codemode.AdapterOpenCodeGo, route.Models...), ClientRequest{GatewayURL: "https://gateway.example", Model: "minimax-m3"})
	if err != nil || config.Client != "opencode" || config.Format != "json" || *config.File != "opencode.json" || config.ClientVersion != "1.18.34" || *config.SmallModel != "minimax-m3" {
		t.Fatalf("configuration: %+v %v", config, err)
	}
	want := `{
  "$schema": "https://opencode.ai/config.json",
  "model": "opencode-go/minimax-m3",
  "small_model": "opencode-go/minimax-m3",
  "enabled_providers": [
    "opencode-go"
  ],
  "provider": {
    "opencode-go": {
      "options": {
        "baseURL": "https://gateway.example/code/team-go/v1",
        "apiKey": "{env:OLP_API_KEY}"
      },
      "whitelist": [
        "kimi-k3",
        "minimax-m3"
      ]
    }
  }
}
`
	if config.Configuration != want {
		t.Fatalf("configuration:\n%s", config.Configuration)
	}
	zai, err := ClientConfiguration(codemode.Route{Slug: "team-glm", Enabled: true, RevisionID: "published", Models: []string{"glm-5.3"}}, offer(codemode.AdapterZAICoding, "glm-5.3"), ClientRequest{GatewayURL: "https://gateway.example", Client: "opencode"})
	if err != nil || !strings.Contains(zai.Configuration, `"zai-coding-plan/glm-5.3"`) || !strings.Contains(zai.Configuration, `"baseURL": "https://gateway.example/code/team-glm/v1"`) {
		t.Fatalf("GLM OpenCode configuration: %v\n%s", err, zai.Configuration)
	}
}

func TestClientConfigurationRefusesUnsupportedSelections(t *testing.T) {
	route := codemode.Route{Slug: "team", Enabled: true, RevisionID: "published", Models: []string{"glm-5.3"}}
	for _, test := range []struct {
		adapter codemode.Adapter
		request ClientRequest
		field   string
	}{
		{codemode.AdapterZAICoding, ClientRequest{Client: "codex"}, "client"},
		{codemode.AdapterCodex, ClientRequest{Client: "claude-code"}, "client"},
		{codemode.AdapterOpenCodeGo, ClientRequest{Client: "cline"}, "client"},
		{codemode.AdapterCodex, ClientRequest{SmallModel: "glm-5.3"}, "small_model"},
		{codemode.AdapterCodex, ClientRequest{PlanModel: "glm-5.3"}, "plan_model"},
		{codemode.AdapterZAICoding, ClientRequest{PlanModel: "glm-4.7"}, "plan_model"},
		{codemode.AdapterZAICoding, ClientRequest{SmallModel: "glm-4.7"}, "small_model"},
		{codemode.AdapterZAICoding, ClientRequest{Model: "claude-sonnet-4-6"}, "model"},
	} {
		test.request.GatewayURL = "https://gateway.example"
		_, err := ClientConfiguration(route, offer(test.adapter, route.Models...), test.request)
		if problem, ok := errors.AsType[*access.Problem](err); !ok || problem.Field != test.field {
			t.Fatalf("%s %+v: %v", test.adapter, test.request, err)
		}
	}
	if _, err := ClientConfiguration(route, offer("unknown", route.Models...), ClientRequest{GatewayURL: "https://gateway.example"}); err == nil {
		t.Fatal("unknown adapter generated configuration")
	}
	for _, adapter := range []codemode.Adapter{codemode.AdapterCodex, codemode.AdapterOpenCodeGo, codemode.AdapterZAICoding} {
		v, _ := codeadapter.Lookup(adapter)
		for _, client := range v.Clients {
			config, err := ClientConfiguration(route, offer(adapter, route.Models...), ClientRequest{GatewayURL: "https://gateway.example", Client: client})
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"olp_", "api.z.ai", "open.bigmodel.cn", "opencode.ai/zen", "chatgpt.com", "refresh_token"} {
				if strings.Contains(config.Configuration, forbidden) {
					t.Fatalf("%s %s configuration contains %s", adapter, client, forbidden)
				}
			}
		}
	}
}

func TestMixedRouteConfigurationsDrawEachModelFromItsSubscription(t *testing.T) {
	route := codemode.Route{Slug: "team-mix", Enabled: true, RevisionID: "published", Models: []string{"glm-5.3", "gpt-5.5", "minimax-m3"}}
	offers := Offers{
		"glm-5.3":    {codemode.AdapterZAICoding},
		"gpt-5.5":    {codemode.AdapterCodex, codemode.AdapterOpenCodeGo},
		"minimax-m3": {codemode.AdapterOpenCodeGo},
	}
	claude, err := ClientConfiguration(route, offers, ClientRequest{GatewayURL: "https://gateway.example", PlanModel: "minimax-m3"})
	if err != nil || claude.Client != "claude-code" || !reflect.DeepEqual(claude.SupportedClients, []string{"claude-code", "opencode", "codex"}) ||
		!reflect.DeepEqual(claude.Adapters, []codemode.Adapter{codemode.AdapterOpenCodeGo, codemode.AdapterZAICoding}) || !reflect.DeepEqual(claude.NativeModels, route.Models) {
		t.Fatalf("Claude Code configuration: %+v %v", claude, err)
	}
	if want := `# Claude Code 2.1.286 for OLP code-mode route team-mix (OpenCode Go and GLM Coding Plan).
# Source this file in a POSIX shell, such as bash or zsh, before starting claude.
# Set OLP_API_KEY to your OLP inference key first. No Anthropic login or vendor
# key belongs on this machine. To select other models, regenerate this file.
unset ANTHROPIC_API_KEY ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL='https://gateway.example/code/team-mix'
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_MODEL='opusplan'
export ANTHROPIC_DEFAULT_OPUS_MODEL='minimax-m3'
export ANTHROPIC_DEFAULT_SONNET_MODEL='glm-5.3'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='glm-5.3'
export CLAUDE_CODE_SUBAGENT_MODEL='glm-5.3'
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
`; claude.Configuration != want {
		t.Fatalf("Claude Code configuration:\n%s", claude.Configuration)
	}
	if !slices.ContainsFunc(claude.QualificationGaps, func(gap string) bool { return strings.HasPrefix(gap, "Each request carries the conversation so far") }) {
		t.Fatalf("mixed route gaps: %v", claude.QualificationGaps)
	}
	opencode, err := ClientConfiguration(route, offers, ClientRequest{GatewayURL: "https://gateway.example", Client: "opencode", PlanModel: "gpt-5.5"})
	if err != nil || *opencode.PlanModel != "gpt-5.5" || *opencode.SmallModel != "glm-5.3" {
		t.Fatalf("OpenCode configuration: %+v %v", opencode, err)
	}
	if want := `{
  "$schema": "https://opencode.ai/config.json",
  "model": "zai-coding-plan/glm-5.3",
  "small_model": "zai-coding-plan/glm-5.3",
  "agent": {
    "plan": {
      "model": "opencode-go/gpt-5.5"
    }
  },
  "enabled_providers": [
    "opencode-go",
    "zai-coding-plan"
  ],
  "provider": {
    "opencode-go": {
      "options": {
        "baseURL": "https://gateway.example/code/team-mix/v1",
        "apiKey": "{env:OLP_API_KEY}"
      },
      "whitelist": [
        "gpt-5.5",
        "minimax-m3"
      ]
    },
    "zai-coding-plan": {
      "options": {
        "baseURL": "https://gateway.example/code/team-mix/v1",
        "apiKey": "{env:OLP_API_KEY}"
      },
      "whitelist": [
        "glm-5.3"
      ]
    }
  }
}
`; opencode.Configuration != want {
		t.Fatalf("OpenCode configuration:\n%s", opencode.Configuration)
	}
	codex, err := ClientConfiguration(route, offers, ClientRequest{GatewayURL: "https://gateway.example", Client: "codex"})
	if err != nil || codex.Model != "gpt-5.5" || !reflect.DeepEqual(codex.NativeModels, []string{"gpt-5.5"}) || !reflect.DeepEqual(codex.Adapters, []codemode.Adapter{codemode.AdapterCodex}) || codex.PlanModel != nil {
		t.Fatalf("Codex configuration: %+v %v", codex, err)
	}
	if _, err := ClientConfiguration(route, offers, ClientRequest{GatewayURL: "https://gateway.example", Client: "codex", Model: "glm-5.3"}); err == nil {
		t.Fatal("Codex configured with a model no Codex account serves")
	}
	_, err = ClientConfiguration(route, offers, ClientRequest{GatewayURL: "https://gateway.example", Client: "cline"})
	if problem, ok := errors.AsType[*access.Problem](err); !ok || problem.Detail != "This route's accounts support Claude Code, OpenCode and Codex." {
		t.Fatalf("unsupported client: %v", err)
	}
}

// offer is the offers of a route whose accounts all belong to one adapter.
func offer(adapter codemode.Adapter, models ...string) Offers {
	offers := Offers{}
	for _, model := range models {
		offers[model] = []codemode.Adapter{adapter}
	}
	return offers
}

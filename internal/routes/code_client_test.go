package routes

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
)

func TestMixedRouteConfigurationsDrawEachModelFromItsSubscription(t *testing.T) {
	route := codemode.Route{Slug: "team-mix", Enabled: true, RevisionID: "published", Models: []string{"glm-5.3", "gpt-5.5", "minimax-m3"}}
	offers := Offers{
		{Adapter: codemode.AdapterOpenCodeGo, Models: []string{"gpt-5.5", "minimax-m3"}},
		{Adapter: codemode.AdapterZAICoding, Models: []string{"glm-5.3"}},
		{Adapter: codemode.AdapterCodex, Models: []string{"gpt-5.5"}},
	}
	claude, err := ClientConfiguration(route, offers, ClientRequest{GatewayURL: "https://gateway.example", PlanModel: "minimax-m3"})
	if err != nil || claude.Client != "claude-code" || !reflect.DeepEqual(claude.SupportedClients, []string{"claude-code", "opencode", "codex"}) ||
		!reflect.DeepEqual(claude.Adapters, []codemode.Adapter{codemode.AdapterOpenCodeGo, codemode.AdapterZAICoding}) || !reflect.DeepEqual(claude.NativeModels, []string{"glm-5.3", "minimax-m3"}) {
		t.Fatalf("Claude Code configuration: %+v %v", claude, err)
	}
	// No subscription serves GPT on Messages, so Claude Code cannot plan with it.
	if _, err := ClientConfiguration(route, offers, ClientRequest{GatewayURL: "https://gateway.example", PlanModel: "gpt-5.5"}); err == nil {
		t.Fatal("Claude Code planned with a Responses model")
	} else if problem, ok := errors.AsType[*access.Problem](err); !ok || problem.Field != "plan_model" {
		t.Fatalf("Claude Code GPT plan: %v", err)
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
	return Offers{{Adapter: adapter, Models: models}}
}

// Two OpenCode Go accounts split their models, and a conversation uses
// only one of them.

// Sent through Z.ai, qwen-4 goes on Chat Completions, which the other
// OpenCode Go account does not serve it on, so a conversation reaches it.

// OpenCode Go serves GPT only on Responses, which Z.ai does not.

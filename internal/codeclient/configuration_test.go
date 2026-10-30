package codeclient

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func TestWindowsClaudeConfiguration(t *testing.T) {
	route := codemode.Route{Slug: "team-code", Enabled: true, RevisionID: "published", Models: []string{"minimax-m3"}}
	offers := Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: route.Models}}
	config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example/prefix's", Client: "claude-code", Platform: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if config.Platform != "windows" || config.Format != "powershell" || config.File == nil || *config.File != "olp-claude.ps1" {
		t.Fatalf("unexpected Windows setup: %+v", config)
	}
	for _, setting := range []string{
		"$env:ANTHROPIC_AUTH_TOKEN = $env:OLP_API_KEY",
		"$env:ANTHROPIC_BASE_URL = '" + config.BaseURL + "'",
		"$env:ANTHROPIC_MODEL = 'minimax-m3'",
	} {
		if !strings.Contains(config.Configuration, setting) {
			t.Fatalf("missing %q in %s", setting, config.Configuration)
		}
	}
	if !strings.Contains(config.Instructions, `. .\olp-claude.ps1`) || !strings.Contains(config.Instructions, "settings.json") {
		t.Fatalf("missing launch and preservation instructions: %s", config.Instructions)
	}
}

func TestPlatformPathsAndInvalidPlatform(t *testing.T) {
	route := codemode.Route{Slug: "team-code", Enabled: true, RevisionID: "published", Models: []string{"fixture-model"}}
	offers := Offers{{Adapter: codemode.AdapterCodex, Models: route.Models}}
	for _, test := range []struct {
		platform, path, fallback string
	}{
		{"", "$CODEX_HOME/config.toml", "$HOME/.codex/config.toml"},
		{"linux", "$CODEX_HOME/config.toml", "$HOME/.codex/config.toml"},
		{"macos", "$CODEX_HOME/config.toml", "$HOME/.codex/config.toml"},
		{"windows", `$env:CODEX_HOME\config.toml`, `$HOME\.codex\config.toml`},
	} {
		t.Run(test.platform, func(t *testing.T) {
			config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example", Platform: test.platform})
			if err != nil {
				t.Fatal(err)
			}
			if config.File == nil || *config.File != test.path || !strings.Contains(config.Instructions, test.fallback) {
				t.Fatalf("wrong config paths: %+v", config)
			}
			if test.platform == "" && config.Platform != "linux" {
				t.Fatalf("unexpected default platform: %s", config.Platform)
			}
			if !strings.Contains(config.Configuration, `model = "`+config.Model+`"`) || !strings.Contains(config.Configuration, `"X-OLP-Code-Model" = "`+config.Model+`"`) {
				t.Fatalf("model and WebSocket hint disagree: %s", config.Configuration)
			}
		})
	}
	_, err := Generate(route, offers, Request{GatewayURL: "https://olp.example", Platform: "unsupported"})
	var problem *access.Problem
	if !errors.As(err, &problem) || problem.Status != 422 || problem.Field != "platform" {
		t.Fatalf("invalid platform accepted: %v", err)
	}
}

func TestClaudeShellPreservesSettingsAndSelectedModels(t *testing.T) {
	models := []string{"minimax-test-main", "minimax-test-plan", "minimax-test-small"}
	route := codemode.Route{Slug: "team-code", Enabled: true, RevisionID: "published", Models: models}
	offers := Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: models}}
	config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example/quote's", Client: "claude-code", Platform: "linux", Model: models[0], PlanModel: models[1], SmallModel: models[2]})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	settings := []byte(`{"permissions":{"allow":["Read"]},"theme":"dark"}`)
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, *config.File), []byte(config.Configuration), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", `. ./olp-claude.sh; printf '%s\n' "$ANTHROPIC_BASE_URL" "$ANTHROPIC_AUTH_TOKEN" "$ANTHROPIC_MODEL" "$ANTHROPIC_DEFAULT_OPUS_MODEL" "$ANTHROPIC_DEFAULT_SONNET_MODEL" "$ANTHROPIC_DEFAULT_HAIKU_MODEL" "$CLAUDE_CODE_SUBAGENT_MODEL" "${ANTHROPIC_API_KEY-unset}" "${CLAUDE_CODE_USE_BEDROCK-unset}"`)
	command.Dir = directory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "OLP_API_KEY=fixture-key-$not-expanded", "ANTHROPIC_API_KEY=stale-credential", "CLAUDE_CODE_USE_BEDROCK=1"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("source generated setup: %v: %s", err, output)
	}
	want := strings.Join([]string{config.BaseURL, "fixture-key-$not-expanded", "opusplan", models[1], models[0], models[2], models[0], "unset", "unset", ""}, "\n")
	if string(output) != want {
		t.Fatalf("wrong sourced environment: %q", output)
	}
	preserved, err := os.ReadFile(filepath.Join(directory, "settings.json"))
	if err != nil || string(preserved) != string(settings) {
		t.Fatalf("existing settings changed: %q: %v", preserved, err)
	}
}

func TestOpenCodeSelectionReplacesOnlyPlanningModel(t *testing.T) {
	models := []string{"minimax-test-main", "minimax-test-plan"}
	route := codemode.Route{Slug: "team-code", Enabled: true, RevisionID: "published", Models: models}
	offers := Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: models}}
	type agent struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	type agents struct {
		Plan   agent `json:"plan"`
		Review agent `json:"review"`
	}
	settings := struct {
		Model        string   `json:"model"`
		Agent        agents   `json:"agent"`
		Instructions []string `json:"instructions"`
	}{Agent: agents{Plan: agent{Prompt: "Keep the planning prompt"}, Review: agent{Model: "custom/reviewer", Prompt: "Keep the review prompt"}}, Instructions: []string{"Keep project conventions"}}
	initial, initialErr := Generate(route, offers, Request{GatewayURL: "https://olp.example", Client: "opencode", Model: models[0], PlanModel: models[1]})
	if initialErr != nil {
		t.Fatal(initialErr)
	}
	if err := json.Unmarshal([]byte(initial.Configuration), &settings); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(settings.Agent.Plan.Model, "/"+models[1]) || settings.Agent.Plan.Model == settings.Model {
		t.Fatal("distinct planning selection did not take effect")
	}
	updated, updatedErr := Generate(route, offers, Request{GatewayURL: "https://olp.example", Client: "opencode", Model: models[0], PlanModel: models[0]})
	if updatedErr != nil {
		t.Fatal(updatedErr)
	}
	if err := json.Unmarshal([]byte(updated.Configuration), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Agent.Plan.Model != settings.Model {
		t.Fatalf("stale planning override after selecting main model: %s != %s", settings.Agent.Plan.Model, settings.Model)
	}
	if settings.Agent.Plan.Prompt != "Keep the planning prompt" || settings.Agent.Review != (agent{Model: "custom/reviewer", Prompt: "Keep the review prompt"}) || len(settings.Instructions) != 1 || settings.Instructions[0] != "Keep project conventions" {
		t.Fatalf("unrelated settings changed: %+v", settings)
	}
}

func TestGenerateOpenCodeCatalog(t *testing.T) {
	models := []string{"private-code-v42", "qwen-private-v42", "gpt-private-v42"}
	route := codemode.Route{Slug: "coding", RevisionID: "revision", Enabled: true, Models: append(models, "unavailable-model")}
	offers := Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: append(models, "unpublished-model")}}
	config, err := Generate(route, offers, Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Models, Catalog(route, offers)) || !reflect.DeepEqual(config.NativeModels, models) {
		t.Fatalf("configuration differs from reachable catalog %+v", config)
	}
	var document struct {
		Model    string `json:"model"`
		Provider map[string]struct {
			Whitelist []string `json:"whitelist"`
			Models    map[string]struct {
				ID       string `json:"id"`
				Provider struct {
					NPM string `json:"npm"`
				} `json:"provider"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(config.Configuration), &document); err != nil {
		t.Fatal(err)
	}
	provider := document.Provider["opencode-go"]
	if document.Model != "opencode-go/"+models[0] || !reflect.DeepEqual(provider.Whitelist, models) || len(provider.Models) != len(models) {
		t.Fatalf("models absent from client catalog %+v", document)
	}
	// OpenCode's model.provider.npm contract, checked 2026-10-30: https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/provider/provider.ts
	for id, npm := range map[string]string{
		models[0]: "@ai-sdk/openai-compatible",
		models[1]: "@ai-sdk/anthropic",
		models[2]: "@ai-sdk/openai",
	} {
		if model := provider.Models[id]; model.ID != id || model.Provider.NPM != npm {
			t.Fatalf("model %s cannot use its native protocol %+v", id, model)
		}
	}
}

func TestGenerateKeepsDisjointAccountModelsSeparate(t *testing.T) {
	models := []string{"private-code-a", "private-code-b"}
	route := codemode.Route{Slug: "coding", RevisionID: "revision", Enabled: true, Models: models}
	offers := Offers{
		{Adapter: codemode.AdapterZAICoding, Models: models[:1]},
		{Adapter: codemode.AdapterZAICoding, Models: models[1:]},
	}
	for _, model := range models {
		config, err := Generate(route, offers, Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode, Model: model})
		if err != nil || config.Model != model || !reflect.DeepEqual(config.NativeModels, models) {
			t.Fatalf("independently reachable model %s, config %+v, error %v", model, config, err)
		}
	}
	if _, err := Generate(route, offers, Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode, Model: models[0], PlanModel: models[1]}); err == nil {
		t.Fatal("configuration combined disjoint accounts of the same subscription")
	}
}

func TestClaudeConfigurationThroughCodexRequiresTranslationPolicy(t *testing.T) {
	models := []string{"fixture-main", "fixture-plan"}
	offers := Offers{{Adapter: codemode.AdapterCodex, Models: models}}
	for _, enabled := range []bool{false, true} {
		route := codemode.Route{Slug: "team-code", Enabled: true, RevisionID: "published", Models: models, MessagesToCodex: enabled}
		config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example", Client: "claude-code", Model: models[0], PlanModel: models[1]})
		if !enabled {
			var problem *access.Problem
			if !errors.As(err, &problem) || problem.Field != "client" {
				t.Fatalf("native route allowed translated setup: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if config.Client != "claude-code" || config.Model != models[0] || config.PlanModel == nil || *config.PlanModel != models[1] || config.SmallModel == nil || *config.SmallModel != models[0] {
			t.Fatalf("translated model selection lost: %+v", config)
		}
		if !strings.Contains(config.Configuration, "ANTHROPIC_BASE_URL") || !strings.Contains(config.Instructions, "tool use") || !strings.Contains(config.Instructions, "reasoning effort") || !strings.Contains(config.Instructions, "identity") {
			t.Fatalf("missing translated setup or limitations: %+v", config)
		}
		defaults, err := Generate(route, offers, Request{GatewayURL: "https://olp.example"})
		if err != nil || defaults.Client != "codex" {
			t.Fatalf("translation changed native default: %+v, %v", defaults, err)
		}
	}
}

func TestClaudeTranslatedCompanionsRequireReachableAccounts(t *testing.T) {
	models := []string{"gpt-fixture-main", "gpt-fixture-plan"}
	route := codemode.Route{Slug: "coding", RevisionID: "revision", Enabled: true, Models: models, MessagesToCodex: true}
	for _, test := range []struct {
		name     string
		offers   Offers
		accepted bool
	}{
		{name: "disjoint Codex accounts with unrelated Responses account", offers: Offers{
			{Adapter: codemode.AdapterCodex, Models: models[:1]},
			{Adapter: codemode.AdapterCodex, Models: models[1:]},
			{Adapter: codemode.AdapterOpenCodeGo, Models: models},
		}},
		{name: "one Codex account serves both models", offers: Offers{
			{Adapter: codemode.AdapterCodex, Models: models},
			{Adapter: codemode.AdapterOpenCodeGo, Models: models},
		}, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := Generate(route, test.offers, Request{GatewayURL: "https://olp.example", Client: codeadapter.ClientClaudeCode, Model: models[0], PlanModel: models[1]})
			if test.accepted {
				if err != nil || config.PlanModel == nil || *config.PlanModel != models[1] {
					t.Fatalf("reachable companions refused, config %+v, error %v", config, err)
				}
				return
			}
			var problem *access.Problem
			if !errors.As(err, &problem) || problem.Field != "plan_model" {
				t.Fatalf("unreachable companions accepted, config %+v, error %v", config, err)
			}
		})
	}
}

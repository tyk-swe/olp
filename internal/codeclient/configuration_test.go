package codeclient

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

func TestExactAliasesAppearInDiscoveryAndGeneratedSetup(t *testing.T) {
	unavailableRoute := codemode.Route{Slug: "coding", Enabled: true, RevisionID: "published", Models: []string{"minimax-fixture-model"}, ModelRewriting: true, MessagesToCodex: true, Aliases: map[string]string{"team-code": "minimax-fixture-model", "offline": "unavailable"}}
	route := codemode.Route{Slug: unavailableRoute.Slug, Enabled: true, RevisionID: unavailableRoute.RevisionID, Models: unavailableRoute.Models, ModelRewriting: true, MessagesToCodex: true, Aliases: map[string]string{"team-code": "minimax-fixture-model"}}
	offers := Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: route.Models}}
	catalog := Catalog(unavailableRoute, offers)
	if len(catalog) != 2 || catalog[1].ID != "team-code" || !reflect.DeepEqual(catalog[0].Capabilities, catalog[1].Capabilities) || !reflect.DeepEqual(catalog[0].Clients, catalog[1].Clients) {
		t.Fatalf("alias discovery lost the target's capabilities %+v", catalog)
	}
	for _, client := range []string{"claude-code", "opencode"} {
		config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example", Client: client, Model: "team-code"})
		if err != nil || config.Model != "team-code" || !strings.Contains(config.Configuration, "team-code") {
			t.Fatalf("alias setup for %s %+v, %v", client, config, err)
		}
		if client == "opencode" {
			var document struct {
				Provider map[string]struct {
					Models map[string]struct {
						Provider struct {
							NPM string `json:"npm"`
						} `json:"provider"`
					} `json:"models"`
				} `json:"provider"`
			}
			if err := json.Unmarshal([]byte(config.Configuration), &document); err != nil {
				t.Fatal(err)
			}
			for _, provider := range document.Provider {
				if provider.Models["team-code"].Provider.NPM != provider.Models["minimax-fixture-model"].Provider.NPM {
					t.Fatalf("alias and native model use different protocols %s", config.Configuration)
				}
			}
		}
	}
	codexOffers := Offers{{Adapter: codemode.AdapterCodex, Models: route.Models}}
	for _, client := range []string{"codex", "claude-code"} {
		config, err := Generate(route, codexOffers, Request{GatewayURL: "https://olp.example", Client: client, Model: "team-code"})
		if err != nil || config.Model != "team-code" || !strings.Contains(config.Configuration, "team-code") {
			t.Fatalf("alias setup for %s %+v, %v", client, config, err)
		}
	}
}

func TestModelPoolsAreVisibleWithoutChangingSetupModel(t *testing.T) {
	route := codemode.Route{Slug: "coding", Enabled: true, RevisionID: "published", Models: []string{"gpt-fixture-main", "gpt-fixture-alternate"}, ModelRewriting: true, InitialRecoveryAttempts: 1, Aliases: map[string]string{"team-code": "gpt-fixture-main"}, ModelPools: map[string][]string{"team-code": {"gpt-fixture-main", "gpt-fixture-alternate"}}}
	offers := Offers{{Adapter: codemode.AdapterCodex, Models: route.Models}}
	configuration, err := Generate(route, offers, Request{GatewayURL: "https://olp.example", Client: "codex", Model: "team-code"})
	if err != nil || configuration.Model != "team-code" || !strings.Contains(configuration.Configuration, "team-code") || !strings.Contains(configuration.Instructions, "model pools") {
		t.Fatalf("model pool setup changed public selection or hid policy %+v, %v", configuration, err)
	}
	model := slices.IndexFunc(configuration.Models, func(model codemode.Model) bool { return model.ID == "team-code" })
	if model < 0 || !slices.Equal(configuration.Models[model].ModelPool, route.ModelPools["team-code"]) {
		t.Fatalf("discovery lost configured targets %+v", configuration.Models)
	}
}

func TestCatalogAndConfigurationExposeDirectPaidModelsWithoutAdapters(t *testing.T) {
	route := codemode.Route{ID: "route", Slug: "coding", Enabled: true, RevisionID: "revision", Models: []string{"paid-native"}, PaidParticipation: true, PaidTargets: map[string]codemode.PaidTarget{"paid-native": {ProviderID: "provider", SlotID: "slot"}}}
	offers := Offers{{PaidProtocol: codemode.ProtocolResponses, Models: route.Models}, {Adapter: codemode.AdapterCodex, Models: route.Models}}
	catalog := Catalog(route, offers)
	if len(catalog) != 1 || !catalog[0].Paid || len(catalog[0].Capabilities) != 1 || catalog[0].Capabilities[0].Protocol != codemode.ProtocolResponses || !slices.Contains(catalog[0].Clients, codeadapter.ClientCodex) {
		t.Fatalf("paid catalog %+v", catalog)
	}
	config, err := Generate(route, offers, Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientCodex, Model: "paid-native"})
	if err != nil || len(config.Adapters) != 0 || !strings.Contains(config.Configuration, "supports_websockets = false") || !slices.Equal(config.PaidModels, route.Models) {
		t.Fatalf("paid configuration %+v %v", config, err)
	}
	openCode, openErr := Generate(route, offers, Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode, Model: "paid-native"})
	if openErr != nil || !strings.Contains(openCode.Configuration, "olp-paid-responses/paid-native") {
		t.Fatalf("paid OpenCode configuration %+v %v", openCode, openErr)
	}
	disabled := route
	disabled.PaidParticipation = false
	if got := Catalog(disabled, offers); len(got) != 0 {
		t.Fatalf("disabled paid target advertised through subscription %+v", got)
	}
}

func TestClaudeSubscriptionSetupUsesNativeModelsAndReportsLiveGap(t *testing.T) {
	models := []string{"claude-fixture-main", "claude-fixture-small"}
	route := codemode.Route{Slug: "personal-code", Enabled: true, RevisionID: "published", Models: models}
	configuration, err := Generate(route, Offers{{Adapter: codemode.AdapterClaude, Models: models}}, Request{GatewayURL: "https://olp.example", Client: "claude-code", Model: models[0], SmallModel: models[1]})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Model != models[0] || !strings.Contains(configuration.Configuration, "ANTHROPIC_AUTH_TOKEN") || !strings.Contains(configuration.Configuration, models[1]) {
		t.Fatal("native setup lost the selected models or gateway credential")
	}
	if !slices.ContainsFunc(configuration.QualificationGaps, func(gap string) bool { return strings.Contains(gap, "Live subscription inference is not qualified") }) {
		t.Fatal("fixture-only subscription was advertised as qualified")
	}
}

func TestAntigravitySetupRequiresExplicitPolicyAndReportsSignedStateGap(t *testing.T) {
	offers := Offers{{Adapter: codemode.AdapterAntigravity, Models: []string{"gemini-fixture"}}}
	route := codemode.Route{ID: "route", ProjectID: "project", Slug: "coding", PoolID: "pool", Models: []string{"gemini-fixture"}, Enabled: true, RevisionID: "published"}
	request := Request{Client: codeadapter.ClientOpenCode, GatewayURL: "https://gateway.invalid", Model: "gemini-fixture"}
	if _, err := Generate(route, offers, request); err == nil {
		t.Fatal("disabled target produced setup")
	}
	enabled := codemode.Route{ID: route.ID, ProjectID: route.ProjectID, Slug: route.Slug, PoolID: route.PoolID, Models: route.Models, Enabled: true, RevisionID: "published", ChatToAntigravity: true}
	configuration, err := Generate(enabled, offers, request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configuration.Configuration, `"olp-antigravity"`) || !strings.Contains(configuration.Configuration, `"@ai-sdk/openai-compatible"`) || !strings.Contains(configuration.Configuration, `https://gateway.invalid/code/coding/v1`) || configuration.Model != "gemini-fixture" {
		t.Fatalf("OpenCode setup lost the existing Chat client mapping %+v", configuration)
	}
	notes := strings.ToLower(strings.Join(configuration.QualificationGaps, " "))
	if !strings.Contains(notes, "signed") || !strings.Contains(notes, "live") || !strings.Contains(notes, "pending") {
		t.Fatalf("setup omitted qualification gaps %s", notes)
	}
	catalog := Catalog(route, offers)
	for _, model := range catalog {
		if len(model.Capabilities) > 0 || len(model.Clients) > 0 {
			t.Fatalf("disabled translation advertised capability %+v", model)
		}
	}
}

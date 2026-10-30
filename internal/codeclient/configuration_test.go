package codeclient

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
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
		"if ([string]::IsNullOrWhiteSpace($env:OLP_API_KEY))",
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
	for _, platform := range []string{"", "linux", "macos", "windows"} {
		t.Run(platform, func(t *testing.T) {
			config, err := Generate(route, offers, Request{GatewayURL: "https://olp.example", Platform: platform})
			if err != nil {
				t.Fatal(err)
			}
			path := "$CODEX_HOME/config.toml"
			fallback := "$HOME/.codex/config.toml"
			if platform == "windows" {
				path = `$env:CODEX_HOME\config.toml`
				fallback = `$HOME\.codex\config.toml`
			}
			if config.File == nil || *config.File != path || !strings.Contains(config.Instructions, fallback) {
				t.Fatalf("wrong config paths: %+v", config)
			}
			if platform == "" && config.Platform != "linux" {
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

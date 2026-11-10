package client

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/code"
)

func codexConfiguration(baseURL, model string, websockets bool) string {
	return fmt.Sprintf(`model_provider = "olp"
model = %s

[model_providers.olp]
name = "OpenAI"
base_url = %s
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = %t
http_headers = { "X-OLP-Code-Model" = %s }
`, strconv.Quote(model), strconv.Quote(baseURL), websockets, strconv.Quote(model))
}

const claudeCredentialVariables = "ANTHROPIC_API_KEY ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY"

type claudeSetting struct {
	name, value string
}

func claudeSettings(baseURL, model, plan, small string) []claudeSetting {
	return []claudeSetting{
		{"ANTHROPIC_BASE_URL", baseURL},
		{"ANTHROPIC_MODEL", claudeModel(model, plan)},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL", plan},
		{"ANTHROPIC_DEFAULT_SONNET_MODEL", model},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL", small},
		{"CLAUDE_CODE_SUBAGENT_MODEL", model},
		{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"},
	}
}

func claudeCodeConfiguration(baseURL, model, plan, small string) string {
	settings := slices.Collect(func(yield func(string) bool) {
		for _, setting := range claudeSettings(baseURL, model, plan, small) {
			if !yield("export " + setting.name + "=" + shellQuote(setting.value)) {
				return
			}
		}
	})
	return `: "${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
` + "unset " + claudeCredentialVariables + `
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
` + strings.Join(settings, "\n") + "\n"
}

func claudeCodePowerShellConfiguration(baseURL, model, plan, small string) string {
	settings := slices.Collect(func(yield func(string) bool) {
		for _, setting := range claudeSettings(baseURL, model, plan, small) {
			if !yield("$env:" + setting.name + " = " + powerShellQuote(setting.value)) {
				return
			}
		}
	})
	return `if ([string]::IsNullOrWhiteSpace($env:OLP_API_KEY)) {
    Write-Error 'Set OLP_API_KEY to your OLP inference key'
    return
}
` + "Remove-Item Env:" + strings.ReplaceAll(claudeCredentialVariables, " ", ", Env:") + ` -ErrorAction SilentlyContinue
$env:ANTHROPIC_AUTH_TOKEN = $env:OLP_API_KEY
` + strings.Join(settings, "\n") + "\n"
}

func powerShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func shellQuote(s string) string      { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func claudeModel(model, plan string) string {
	if plan != model {
		return "opusplan"
	}
	return model
}

func platformInstructions(client, platform string) (*string, string) {
	const posixKey = "Export OLP_API_KEY in the shell that starts the client. "
	const windowsKey = "Set $env:OLP_API_KEY in the PowerShell session that starts the client. "
	switch client {
	case code.ClientCodex:
		const merge = ", update model and model_provider at the top level and replace only the model_providers.olp table. Merge the generated mcp_servers tables by name, preserving all other settings and servers. Do not append duplicate TOML keys or tables. Run codex. Regenerate this setup when changing models so the WebSocket model header stays in sync."
		if platform == code.PlatformWindows {
			return new(`$env:CODEX_HOME\config.toml`), windowsKey + `In $env:CODEX_HOME\config.toml, or $HOME\.codex\config.toml when CODEX_HOME is unset` + merge
		}
		return new("$CODEX_HOME/config.toml"), posixKey + "In $CODEX_HOME/config.toml, or $HOME/.codex/config.toml when CODEX_HOME is unset" + merge
	case code.ClientClaudeCode:
		const merge = "This setup does not write your settings.json files. Merge companion mcpServers entries by name into .mcp.json in the project root, preserving other servers. Keep the ${OLP_API_KEY} reference unchanged and approve the project servers in Claude Code. Regenerate this setup to change the main, planning or background models."
		if platform == code.PlatformWindows {
			return new("olp-claude.ps1"), windowsKey + `Save the PowerShell setup as olp-claude.ps1. Run . .\olp-claude.ps1, then run claude in the same PowerShell session. ` + merge
		}
		return new("olp-claude.sh"), posixKey + "Save the shell setup as olp-claude.sh. Run . ./olp-claude.sh in bash or zsh, then run claude in the same shell. " + merge
	case code.ClientOpenCode:
		const merge = "In opencode.json at the project root, update the generated model, small_model, enabled_providers and agent.plan.model fields, including when planning uses the main model. Merge provider entries by name, preserving other plan-agent settings, agents, tools and settings. Merge mcp entries by name and keep {env:OLP_API_KEY} references unchanged. If the project uses opencode.jsonc, merge there instead of creating a competing file. Run opencode from that project. Regenerate this setup to change models."
		if platform == code.PlatformWindows {
			return new("opencode.json"), windowsKey + merge
		}
		return new("opencode.json"), posixKey + merge
	}
	return nil, ""
}

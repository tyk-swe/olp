package codeclient

import (
	"slices"
	"strings"
)

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

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func claudeModel(model, plan string) string {
	if plan != model {
		return "opusplan"
	}
	return model
}

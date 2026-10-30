package codeclient

import (
	"fmt"
	"strings"
)

// claudeCodeConfiguration sets every model variable to a native model the
// route admits, so background, subagent and auto-mode requests stay on it. A
// separate planning model selects opusplan, which plans with the Opus model
// and does everything else with the Sonnet model.
func claudeCodeConfiguration(baseURL, model, plan, small string) string {
	selected := claudeModel(model, plan)
	return fmt.Sprintf(`: "${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
unset ANTHROPIC_API_KEY ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL=%s
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_MODEL=%s
export ANTHROPIC_DEFAULT_OPUS_MODEL=%s
export ANTHROPIC_DEFAULT_SONNET_MODEL=%s
export ANTHROPIC_DEFAULT_HAIKU_MODEL=%s
export CLAUDE_CODE_SUBAGENT_MODEL=%s
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
`, shellQuote(baseURL), shellQuote(selected), shellQuote(plan), shellQuote(model), shellQuote(small), shellQuote(model))
}

func claudeCodePowerShellConfiguration(baseURL, model, plan, small string) string {
	selected := claudeModel(model, plan)
	return fmt.Sprintf(`if ([string]::IsNullOrWhiteSpace($env:OLP_API_KEY)) {
    Write-Error 'Set OLP_API_KEY to your OLP inference key'
    return
}
Remove-Item Env:ANTHROPIC_API_KEY, Env:ANTHROPIC_SMALL_FAST_MODEL, Env:CLAUDE_CODE_USE_BEDROCK, Env:CLAUDE_CODE_USE_VERTEX, Env:CLAUDE_CODE_USE_FOUNDRY -ErrorAction SilentlyContinue
$env:ANTHROPIC_BASE_URL = %s
$env:ANTHROPIC_AUTH_TOKEN = $env:OLP_API_KEY
$env:ANTHROPIC_MODEL = %s
$env:ANTHROPIC_DEFAULT_OPUS_MODEL = %s
$env:ANTHROPIC_DEFAULT_SONNET_MODEL = %s
$env:ANTHROPIC_DEFAULT_HAIKU_MODEL = %s
$env:CLAUDE_CODE_SUBAGENT_MODEL = %s
$env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = '1'
`, powerShellQuote(baseURL), powerShellQuote(selected), powerShellQuote(plan), powerShellQuote(model), powerShellQuote(small), powerShellQuote(model))
}

func powerShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func claudeModel(model, plan string) string {
	if plan != model {
		return "opusplan"
	}
	return model
}

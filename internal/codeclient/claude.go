package codeclient

import (
	"fmt"
	"strings"

	"github.com/tyk-swe/olp/internal/codeadapter"
)

// claudeCodeConfiguration sets every model variable to a native model the
// route admits, so background, subagent and auto-mode requests stay on it. A
// separate planning model selects opusplan, which plans with the Opus model
// and does everything else with the Sonnet model.
func claudeCodeConfiguration(slug, plans, baseURL, model, plan, small string) string {
	selected := model
	if plan != model {
		selected = "opusplan"
	}
	return fmt.Sprintf(`# Claude Code %s for OLP code-mode route %s (%s).
# Source this file in a POSIX shell, such as bash or zsh, before starting claude.
# Set OLP_API_KEY to your OLP inference key first. No Anthropic login or vendor
# key belongs on this machine. To select other models, regenerate this file.
unset ANTHROPIC_API_KEY ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL=%s
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_MODEL=%s
export ANTHROPIC_DEFAULT_OPUS_MODEL=%s
export ANTHROPIC_DEFAULT_SONNET_MODEL=%s
export ANTHROPIC_DEFAULT_HAIKU_MODEL=%s
export CLAUDE_CODE_SUBAGENT_MODEL=%s
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
`, codeadapter.ClaudeCodeVersion, slug, plans, shellQuote(baseURL), shellQuote(selected), shellQuote(plan), shellQuote(model), shellQuote(small), shellQuote(model))
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

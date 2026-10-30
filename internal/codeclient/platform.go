package codeclient

import "github.com/tyk-swe/olp/internal/codeadapter"

func platformInstructions(client, platform string) (*string, string) {
	const posixKey = "Export OLP_API_KEY in the shell that starts the client. "
	const windowsKey = "Set $env:OLP_API_KEY in the PowerShell session that starts the client. "
	switch client {
	case codeadapter.ClientCodex:
		const merge = ", update model and model_provider at the top level and replace only the model_providers.olp table. Merge the generated mcp_servers tables by name, preserving all other settings and servers. Do not append duplicate TOML keys or tables. Run codex. Regenerate this setup when changing models so the WebSocket model header stays in sync."
		if platform == "windows" {
			return new(`$env:CODEX_HOME\config.toml`), windowsKey + `In $env:CODEX_HOME\config.toml, or $HOME\.codex\config.toml when CODEX_HOME is unset` + merge
		}
		return new("$CODEX_HOME/config.toml"), posixKey + "In $CODEX_HOME/config.toml, or $HOME/.codex/config.toml when CODEX_HOME is unset" + merge
	case codeadapter.ClientClaudeCode:
		const merge = "This setup does not write your settings.json files. Merge companion mcpServers entries by name into .mcp.json in the project root, preserving other servers. Keep the ${OLP_API_KEY} reference unchanged and approve the project servers in Claude Code. Regenerate this setup to change the main, planning or background models."
		if platform == "windows" {
			return new("olp-claude.ps1"), windowsKey + `Save the PowerShell setup as olp-claude.ps1. Run . .\olp-claude.ps1, then run claude in the same PowerShell session. ` + merge
		}
		return new("olp-claude.sh"), posixKey + "Save the shell setup as olp-claude.sh. Run . ./olp-claude.sh in bash or zsh, then run claude in the same shell. " + merge
	case codeadapter.ClientOpenCode:
		const merge = "In opencode.json at the project root, update the generated model, small_model and enabled_providers fields. Merge provider entries and the plan agent by name, preserving other agents, tools and settings. Merge mcp entries by name and keep {env:OLP_API_KEY} references unchanged. If the project uses opencode.jsonc, merge there instead of creating a competing file. Run opencode from that project. Regenerate this setup to change models."
		if platform == "windows" {
			return new("opencode.json"), windowsKey + merge
		}
		return new("opencode.json"), posixKey + merge
	}
	return nil, ""
}

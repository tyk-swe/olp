package codeclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func addResearchConfiguration(out *codemode.ClientConfiguration, tools *codemode.ResearchTools) error {
	if tools == nil {
		return nil
	}
	if err := tools.Validate(); err != nil {
		return err
	}
	servers := map[string]any{}
	for _, service := range tools.Services {
		name := "olp_" + out.RouteSlug + "_" + service
		endpoint := out.BaseURL + "/mcp/" + service
		switch out.Client {
		case codeadapter.ClientCodex:
			out.Configuration += fmt.Sprintf("\n[mcp_servers.%s]\nurl = %s\nbearer_token_env_var = \"OLP_API_KEY\"\n", strconv.Quote(name), strconv.Quote(endpoint))
		case codeadapter.ClientClaudeCode:
			servers[name] = map[string]any{"type": "http", "url": endpoint, "headers": map[string]string{"Authorization": "Bearer ${OLP_API_KEY}"}}
		case codeadapter.ClientOpenCode:
			servers[name] = map[string]any{"type": "remote", "url": endpoint, "oauth": false, "headers": map[string]string{"Authorization": "Bearer {env:OLP_API_KEY}"}}
		}
	}
	switch out.Client {
	case codeadapter.ClientClaudeCode:
		configuration, err := configurationJSON(map[string]any{"mcpServers": servers})
		if err != nil {
			return err
		}
		out.AdditionalFiles = []codemode.ConfigurationFile{{File: ".mcp.json", Format: "json", Configuration: configuration}}
	case codeadapter.ClientOpenCode:
		var document map[string]any
		if err := json.Unmarshal([]byte(out.Configuration), &document); err != nil {
			return err
		}
		document["mcp"] = servers
		var err error
		out.Configuration, err = configurationJSON(document)
		return err
	}
	return nil
}

func configurationJSON(value any) (string, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(value)
	return b.String(), err
}

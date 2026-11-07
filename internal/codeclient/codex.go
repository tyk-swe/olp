package codeclient

import (
	"fmt"
	"strconv"
)

func codexConfiguration(baseURL, model string, websockets, grok, compact bool) string {
	compatibility := func() string {
		if !grok {
			return ""
		}
		return `web_search = "disabled"
model_reasoning_summary = "none"
model_reasoning_effort = "none"

[features]
apply_patch_freeform = false
multi_agent = false
goals = false

`
	}()
	compaction := func() string {
		if !compact {
			return ""
		}
		return `
[model_providers.olp.capabilities]
remote_compaction = "v2"
`
	}()
	return fmt.Sprintf(`model_provider = "olp"
model = %s

%s[model_providers.olp]
name = "OpenAI"
base_url = %s
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = %t
http_headers = { "X-OLP-Code-Model" = %s }
`, strconv.Quote(model), compatibility, strconv.Quote(baseURL), websockets, strconv.Quote(model)) + compaction
}

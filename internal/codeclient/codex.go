package codeclient

import (
	"fmt"
	"strconv"
)

func codexConfiguration(baseURL, model string) string {
	return fmt.Sprintf(`model_provider = "olp"
model = %s

[model_providers.olp]
name = "OpenAI"
base_url = %s
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
http_headers = { "X-OLP-Code-Model" = %s }
`, strconv.Quote(model), strconv.Quote(baseURL), strconv.Quote(model))
}

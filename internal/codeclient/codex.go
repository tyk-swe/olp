package codeclient

import (
	"fmt"
	"strconv"
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

package codeclient

import (
	"fmt"
	"strconv"

	"github.com/tyk-swe/olp/internal/codexauth"
)

func codexConfiguration(baseURL, model string) string {
	return fmt.Sprintf(`# Codex %s. Save as $CODEX_HOME/config.toml (default: ~/.codex/config.toml).
# Set OLP_API_KEY to your OLP inference key. No OpenAI login or upstream token is needed.
# To select another model, regenerate with model=... so the WebSocket hint agrees.
model_provider = "olp"
model = %s

[model_providers.olp]
name = "OpenAI"
base_url = %s
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
http_headers = { "X-OLP-Code-Model" = %s }
`, codexauth.ClientVersion, strconv.Quote(model), strconv.Quote(baseURL), strconv.Quote(model))
}

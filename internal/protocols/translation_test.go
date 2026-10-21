package protocols

import (
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

var generationFixtures = map[wire.Format]string{
	wire.Chat:      `{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
	wire.Responses: `{"model":"team-model","input":"hello","max_output_tokens":32}`,
	wire.Anthropic: `{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`,
	wire.Gemini:    `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":32}}`,
}

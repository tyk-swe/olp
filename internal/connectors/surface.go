package connectors

import (
	"cmp"

	"github.com/tyk-swe/olp/internal/domain/vendors"
)

type dialectSurface struct {
	surface string
	only    string
}

var (
	nativeGeneration = dialectSurface{surface: "native", only: "generation"}
	nativeRealtime   = dialectSurface{surface: "native", only: "realtime"}
)

var dialectSurfaces = map[string]dialectSurface{
	completionsDialect:        {surface: "openai"},
	"openai-chat":             {surface: "openai"},
	"openai-responses":        {surface: "openai"},
	"anthropic-messages":      {surface: "anthropic"},
	"gemini-generate-content": {surface: "gemini"},
	"gemini-interactions":     {surface: "gemini", only: "generation"},
	"gemini-live":             {surface: "gemini", only: "realtime"},
	"bedrock-converse":        {surface: "bedrock"},
	"bedrock-invoke":          {surface: "bedrock"},
	"mistral-fim":             nativeGeneration,
	"cohere-chat-v2":          nativeGeneration,
	"xai-realtime":            nativeRealtime,
	"vertex-realtime":         nativeRealtime,
	"bedrock-realtime":        nativeRealtime,
}

var kindSurfaces = map[Kind]string{KindAnthropic: "anthropic", KindGemini: "gemini", KindVertexAI: "gemini"}

func (d dialectSurface) modes() []string {
	if d.only == "realtime" {
		return []string{"realtime"}
	}
	return generating
}

func (p Profile) served() dialectSurface {
	return dialectSurfaces[p.OperationDialect("generation")]
}

func (p Profile) Surface() string { return p.served().surface }

func (c Config) ProbeSurface() string {
	if c.ProfileID == "" {
		return cmp.Or(kindSurfaces[c.Kind], "openai")
	}
	p, _ := c.profile()
	return cmp.Or(p.Surface(), kindSurfaces[c.Kind], "openai")
}

func speaks(vendor string, served dialectSurface) bool {
	for dialect, candidate := range dialectSurfaces {
		if candidate == served && vendors.Speaks(vendor, dialect) {
			return true
		}
	}
	return false
}

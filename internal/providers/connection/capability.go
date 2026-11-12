package connection

import (
	"fmt"
	"slices"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/routing"
)

type Capability struct {
	Operation string `json:"operation"`
	Surface   string `json:"surface"`
	Mode      string `json:"mode"`
}

const maxCapabilities = 64

var vocabulary = []Capability{
	{"generation", "openai", "unary"}, {"generation", "openai", "streaming"}, {"token_count", "openai", "unary"},
	{"generation", "anthropic", "unary"}, {"generation", "anthropic", "streaming"}, {"token_count", "anthropic", "unary"},
	{"generation", "gemini", "unary"}, {"generation", "gemini", "streaming"}, {"token_count", "gemini", "unary"},
	{"batch", "gemini", "unary"},
	{"embeddings", "openai", "unary"}, {"moderation", "openai", "unary"}, {"rerank", "openai", "unary"}, {"translation", "openai", "unary"}, {"decision", "openai", "unary"},
	{"ocr", "openai", "unary"}, {"ocr", "native", "unary"}, {"search", "openai", "unary"}, {"search", "native", "unary"},
	{"image_generation", "openai", "unary"}, {"image_generation", "openai", "streaming"},
	{"image_edit", "openai", "unary"}, {"image_edit", "openai", "streaming"},
	{"speech", "openai", "unary"}, {"speech", "openai", "streaming"},
	{"transcription", "openai", "unary"}, {"transcription", "openai", "streaming"},
	{"image_variation", "openai", "unary"}, {"video_list", "openai", "unary"}, {"video_get", "openai", "unary"}, {"video_content", "openai", "unary"}, {"video_delete", "openai", "unary"},
	{"video_create", "openai", "async"}, {"batch", "openai", "unary"}, {"batch", "anthropic", "unary"},
	{"realtime", "openai", "realtime"}, {"realtime", "openai", "webrtc"}, {"realtime", "gemini", "realtime"}, {"realtime", "native", "realtime"},
	{"generation", "bedrock", "unary"}, {"generation", "bedrock", "streaming"}, {"bedrock_invoke", "bedrock", "unary"}, {"bedrock_invoke", "bedrock", "streaming"},
	{"generation", "native", "unary"}, {"generation", "native", "streaming"}, {"batch", "native", "unary"},
	{"decision", "native", "unary"},
	{"agent_resource", "gemini", "unary"},
	{"vector_store", "openai", "unary"}, {"conversation", "openai", "unary"}, {"container", "openai", "unary"}, {"eval", "openai", "unary"}, {"retrieval", "openai", "unary"},
	{"fine_tuning", "openai", "unary"}, {"fine_tuning", "native", "unary"}, {"skill", "anthropic", "unary"}, {"context_cache", "gemini", "unary"},
}

func Vocabulary() []Capability { return slices.Clone(vocabulary) }

func (c Capability) available() bool {
	return slices.Contains(vocabulary, c) || operationregistry.Catalog().Supports(c.Operation, c.Surface, c.Mode)
}

func ValidCapabilities(declared []Capability) ([]Capability, error) {
	if len(declared) > maxCapabilities {
		return nil, &routing.ValidationError{Field: "capabilities", Message: fmt.Sprintf("Declare at most %d capabilities per model.", maxCapabilities)}
	}
	if i := slices.IndexFunc(declared, func(c Capability) bool { return !c.available() }); i >= 0 {
		c := declared[i]
		return nil, &routing.Refusal{Code: "capability_unavailable", Message: fmt.Sprintf("The %s/%s/%s capability is not available in this release.", c.Operation, c.Surface, c.Mode)}
	}
	var unique []Capability
	for i, c := range declared {
		if !slices.Contains(declared[:i], c) {
			unique = append(unique, c)
		}
	}
	return unique, nil
}

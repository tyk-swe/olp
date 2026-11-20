package publishedprompt

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/prompts"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func gemini(family wire.Format) bool {
	return family == wire.Gemini || family == wire.GeminiStream
}

func encodeMessages(family wire.Format, rendered []prompts.Message) ([]byte, error) {
	if gemini(family) {
		type part struct {
			Text string `json:"text"`
		}
		type content struct {
			Role  string `json:"role"`
			Parts []part `json:"parts"`
		}
		values := slices.Collect(func(yield func(content) bool) {
			for _, message := range rendered {
				if !yield(content{Role: geminiRole(message.Role), Parts: []part{{Text: message.Content}}}) {
					return
				}
			}
		})
		return json.Marshal(values)
	}
	return json.Marshal(rendered)
}

func geminiRole(role string) string {
	if role == "assistant" {
		return "model"
	}
	return role
}

func liftSystem(family wire.Format, messages []prompts.Message) ([]prompts.Message, []oif.Change, error) {
	if len(messages) == 0 || messages[0].Role != "system" || family != wire.Anthropic && !gemini(family) {
		return messages, nil, nil
	}
	value, err := json.Marshal(messages[0].Content)
	if err != nil {
		return nil, nil, err
	}
	if gemini(family) {
		return messages[1:], []oif.Change{{Pointer: "/systemInstruction", Value: `{"parts":[{"text":` + string(value) + `}]}`, Origin: oif.ResourceBinding, Reason: "published prompt rendered"}}, nil
	}
	return messages[1:], []oif.Change{{Pointer: "/system", Value: string(value), Origin: oif.ResourceBinding, Reason: "published prompt rendered"}}, nil
}

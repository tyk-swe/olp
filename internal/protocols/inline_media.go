package protocols

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type InlineMediaLimits struct {
	Items                 int
	ItemBytes, TotalBytes int64
}

// ValidateInlineMedia inspects native media locations without translating the
// request. Native extensions remain valid; prompt text and tool JSON do not count.
func ValidateInlineMedia(request *openai.Request, limits InlineMediaLimits) error {
	if request.Family.Operation() != openai.OperationGeneration && request.Family.Operation() != "token_count" {
		return nil
	}
	doc := request.Document()
	messages, partKey := arr(doc["messages"]), "content"
	if request.Family == openai.FamilyResponses || request.Family == openai.FamilyInputTokens {
		messages = arr(doc["input"])
	}
	switch request.Family {
	case openai.FamilyGemini, openai.FamilyGeminiStream, openai.FamilyGeminiCount:
		content := doc
		if nested, err := object(doc["generateContentRequest"]); err == nil {
			content = nested
		}
		messages, partKey = arr(content["contents"]), "parts"
	}
	count, total := 0, int64(0)
	var visit func(parts []json.RawMessage, depth int) error
	visit = func(parts []json.RawMessage, depth int) error {
		for _, part := range parts {
			p, _ := object(part)
			// Tool results nest their own media: Anthropic tool_result content
			// and Gemini functionResponse parts.
			if depth < 2 {
				var nested []json.RawMessage
				if str(p["type"]) == "tool_result" {
					nested = arr(p["content"])
				} else if response, err := object(p["functionResponse"]); err == nil {
					nested = arr(response["parts"])
				}
				if err := visit(nested, depth+1); err != nil {
					return err
				}
			}
			encoded, dataURL := inlineMediaData(p)
			if dataURL {
				if !strings.HasPrefix(encoded, "data:") {
					continue
				}
				metadata, data, ok := strings.Cut(encoded, ",")
				if !ok || !strings.HasSuffix(metadata, ";base64") {
					return requestError("content", "Inline media must use a base64 data URL.")
				}
				encoded = data
			}
			if encoded == "" {
				continue
			}
			count++
			if count > limits.Items {
				return requestError("content", "Too many inline media items.")
			}
			n, err := io.Copy(io.Discard, io.LimitReader(base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(encoded)), limits.ItemBytes+1))
			if err != nil {
				return requestError("content", "Invalid base64 inline media.")
			}
			total += n
			if n > limits.ItemBytes || total > limits.TotalBytes {
				return requestError("content", "Inline media exceeds the configured decoded byte limit.")
			}
		}
		return nil
	}
	for _, message := range messages {
		m, _ := object(message)
		if err := visit(arr(m[partKey]), 0); err != nil {
			return err
		}
	}
	return nil
}

func inlineMediaData(p Object) (string, bool) {
	nested := func(raw json.RawMessage, key string) string { v, _ := object(raw); return str(v[key]) }
	switch str(p["type"]) {
	case "image_url":
		return nested(p["image_url"], "url"), true
	case "input_image":
		return str(p["image_url"]), true
	case "input_audio":
		return nested(p["input_audio"], "data"), false
	case "input_file":
		return str(p["file_data"]), true
	case "file":
		return nested(p["file"], "file_data"), true
	case "image", "document":
		if nested(p["source"], "type") == "base64" {
			return nested(p["source"], "data"), false
		}
	}
	for _, key := range []string{"inlineData", "inline_data"} {
		if data := nested(p[key], "data"); data != "" {
			return data, false
		}
	}
	return "", false
}

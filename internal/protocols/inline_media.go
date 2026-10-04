package protocols

import (
	"encoding/base64"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type InlineMediaLimits struct {
	Items                 int
	ItemBytes, TotalBytes int64
}

// ValidateInlineMedia inspects native media locations without translating the
// request. Native extensions remain valid; prompt text and tool JSON do not count.
// It reads the request's parsed document, so the prompt text it passes over is
// not scanned again, and the media it decodes is read where it lies.
func ValidateInlineMedia(request *openai.Request, limits InlineMediaLimits) error {
	if request.Family.Operation() != openai.OperationGeneration && request.Family.Operation() != "token_count" {
		return nil
	}
	doc := request.OIF().Document().Root()
	messages, partKey := member(doc, "messages").Elements(), "content"
	if request.Family == openai.FamilyResponses || request.Family == openai.FamilyInputTokens {
		messages = member(doc, "input").Elements()
	}
	switch request.Family {
	case openai.FamilyGemini, openai.FamilyGeminiStream, openai.FamilyGeminiCount:
		content := doc
		if nested := member(doc, "generateContentRequest"); nested.Kind() == oif.Object {
			content = nested
		}
		messages, partKey = member(content, "contents").Elements(), "parts"
	}
	count, total := 0, int64(0)
	var visit func(parts []oif.Value, depth int) error
	visit = func(parts []oif.Value, depth int) error {
		for _, p := range parts {
			// Tool results nest their own media: Anthropic tool_result content
			// and Gemini functionResponse parts.
			if depth < 2 {
				var nested []oif.Value
				if chars(member(p, "type")) == "tool_result" {
					nested = member(p, "content").Elements()
				} else if response := member(p, "functionResponse"); response.Kind() == oif.Object {
					nested = member(response, "parts").Elements()
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
		if err := visit(member(message, partKey).Elements(), 0); err != nil {
			return err
		}
	}
	return nil
}

// member is a member of an object, and no value for one it does not have or for
// a value that is not an object.
func member(v oif.Value, name string) oif.Value {
	m, _ := v.Lookup(name)
	return m
}

// chars is the text of a string, and nothing for any other value.
func chars(v oif.Value) string {
	text, _ := v.Chars()
	return text
}

func inlineMediaData(p oif.Value) (string, bool) {
	nested := func(v oif.Value, key string) string { return chars(member(v, key)) }
	switch chars(member(p, "type")) {
	case "image_url":
		return nested(member(p, "image_url"), "url"), true
	case "input_image":
		return chars(member(p, "image_url")), true
	case "input_audio":
		return nested(member(p, "input_audio"), "data"), false
	case "input_file":
		return chars(member(p, "file_data")), true
	case "file":
		return nested(member(p, "file"), "file_data"), true
	case "image", "document":
		if source := member(p, "source"); nested(source, "type") == "base64" {
			return nested(source, "data"), false
		}
	}
	for _, key := range []string{"inlineData", "inline_data"} {
		if data := nested(member(p, key), "data"); data != "" {
			return data, false
		}
	}
	return "", false
}

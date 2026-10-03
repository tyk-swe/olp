package protocols

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// legacyParse is Parse as it read the conversation of an Anthropic or Gemini
// request when every field of it was json.RawMessage, decoded level by level,
// before it read the request's parsed document instead. The two are held to the
// same answer for any request by request_parity_test.go.
func legacyParse(family openai.Family, data []byte, model string) (*openai.Request, error) {
	if family.Surface() == "openai" {
		return openai.Parse(family, data)
	}
	doc, err := oif.ParseJSON(data, oif.Limits{})
	if err != nil {
		return nil, &openai.RequestError{Code: "invalid_json", Message: "The request body must be unambiguous valid JSON."}
	}
	f := doc.Fields()
	if f == nil {
		return nil, requestError("", "expected a JSON object")
	}
	stream := family == openai.FamilyGeminiStream
	if family.Surface() == "anthropic" {
		model = str(f["model"])
		if present(f["stream"]) {
			if err := json.Unmarshal(f["stream"], &stream); err != nil {
				return nil, requestError("stream", "stream must be a boolean")
			}
		}
		if len(arr(f["messages"])) == 0 {
			return nil, requestError("messages", "messages must be a non-empty array")
		}
		if family.Operation() == "generation" {
			var n int64
			if json.Unmarshal(f["max_tokens"], &n) != nil || n < 1 {
				return nil, requestError("max_tokens", "max_tokens must be a positive integer")
			}
		}
		if err := validateNativeControls(f, false); err != nil {
			return nil, err
		}
		for _, m := range arr(f["messages"]) {
			msg, e := object(m)
			if e != nil {
				return nil, e
			}
			// A system message between turns is the mid-conversation-system beta.
			if role := str(msg["role"]); role != "user" && role != "assistant" && role != "system" {
				return nil, requestError("messages.role", "Anthropic messages require user, assistant or system roles")
			}
			if !present(msg["content"]) {
				return nil, requestError("messages.content", "message content is required")
			}
		}
	} else {
		if _, ok := f["model"]; ok {
			return nil, requestError("model", "Gemini model belongs in the request path")
		}
		if _, ok := f["stream"]; ok {
			return nil, requestError("stream", "Gemini streaming is selected by the request path")
		}
		content := f
		if present(f["generateContentRequest"]) {
			if family != openai.FamilyGeminiCount || present(f["contents"]) {
				return nil, requestError("generateContentRequest", "countTokens accepts either contents or generateContentRequest")
			}
			content, err = object(f["generateContentRequest"])
			if err != nil {
				return nil, err
			}
		}
		if err := validateNativeControls(content, true); err != nil {
			return nil, err
		}
		if len(arr(content["contents"])) == 0 {
			return nil, requestError("contents", "contents must be a non-empty array")
		}
		for _, m := range arr(content["contents"]) {
			msg, e := object(m)
			if e != nil {
				return nil, e
			}
			if len(arr(msg["parts"])) == 0 {
				return nil, requestError("contents.parts", "content parts must be a non-empty array")
			}
		}
	}
	if !openai.RouteSlug.MatchString(model) {
		return nil, requestError("model", "model must name a published route slug")
	}
	if family.Operation() != "generation" && stream {
		return nil, requestError("stream", "token counting supports unary requests only")
	}
	return openai.NewSourceEnvelope(family, model, stream, doc), nil
}

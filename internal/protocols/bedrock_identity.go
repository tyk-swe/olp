package protocols

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// ParseBedrockRequest retains the native Converse document. Its model and
// delivery mode belong to the URL, so neither is introduced into the payload.
func ParseBedrockRequest(body []byte, model string, stream bool) (*openai.Request, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, &openai.RequestError{Code: "invalid_json", Message: "The request body must be unambiguous valid JSON."}
	}
	if !openai.RouteSlug.MatchString(model) {
		return nil, &openai.RequestError{Code: "invalid_value", Param: "model", Message: "The model must name a published route."}
	}
	for _, name := range []string{"model", "stream"} {
		if _, ok := doc.Root().Lookup(name); ok {
			return nil, &openai.RequestError{Code: "invalid_value", Param: name, Message: "Converse model and streaming mode belong to the request path."}
		}
	}
	messages, ok := doc.Root().Lookup("messages")
	if !ok || messages.Kind() != oif.Array || len(messages.Elements()) == 0 {
		return nil, &openai.RequestError{Code: "invalid_value", Param: "messages", Message: "Converse requires a non-empty messages array."}
	}
	for _, message := range messages.Elements() {
		role, _ := message.Lookup("role")
		name, _ := role.Text()
		content, _ := message.Lookup("content")
		if name != "user" && name != "assistant" || content.Kind() != oif.Array || len(content.Elements()) == 0 {
			return nil, &openai.RequestError{Code: "invalid_value", Param: "messages", Message: "Converse messages require user or assistant roles and non-empty content."}
		}
	}
	return openai.NewSourceEnvelope(openai.FamilyBedrock, model, stream, doc), nil
}

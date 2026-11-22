package protocols

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

// Decode validates a bounded upstream result. Native extensions survive model
// rewriting; translated results contain only the shared, represented fields.
func Decode(encodedWire, target wire.Format, body []byte, route, encoding string) (*wire.Completion, error) {
	return decode(encodedWire, target, body, route, encoding, nil)
}

func DecodeRequest(encodedWire, target wire.Format, body []byte, route, encoding string, request *wire.Request) (*wire.Completion, error) {
	return decode(encodedWire, target, body, route, encoding, request)
}

func decode(encodedWire, target wire.Format, body []byte, route, encoding string, request *wire.Request) (*wire.Completion, error) {
	if encodedWire == wire.Chat || encodedWire == wire.Responses {
		return decodeTransformed(encodedWire, target, body, route, encoding, request)
	}
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return nil, protocolError(err.Error())
	}
	result, err := contracts.Result(wire.Descriptor(encodedWire, false), doc, oif.Complete)
	if err != nil {
		return nil, protocolError("unregistered result contract")
	}
	c, err := decodeTransformed(encodedWire, target, result.Source().Bytes(), route, encoding, request)
	if c != nil {
		c.Native = result
		if encodedWire == wire.Bedrock && target == encodedWire {
			// Converse's model belongs to the URL; native output needs no
			// model rewrite. Keep every validated source member verbatim.
			c.Body = result.Source().Bytes()
		}
	}
	return c, err
}

// decodeTransformed is the family result codec. It translates a completion
// when the target dialect differs; strict callers pass wire == target, so it
// only applies the native grammar.
func decodeTransformed(encodedWire, target wire.Format, body []byte, route, encoding string, request *wire.Request) (*wire.Completion, error) {
	c, err := decodeWireCompletion(encodedWire, target, body, route, encoding, request)
	if err != nil {
		return c, err
	}
	if encodedWire == wire.Chat && target == wire.Completions {
		c.Body, err = projectChatCompletion(c.Body, false)
		return c, err
	}
	if encodedWire != target && target.Operation() == "generation" {
		for _, call := range c.ToolCalls {
			if call.ID == "" || call.Name == "" {
				return c, protocolError("incomplete translated tool identity")
			}
			if _, err := object([]byte(call.Arguments)); err != nil {
				return c, protocolError("translated tool arguments must be a JSON object")
			}
		}
		if supportsCandidates(encodedWire) {
			translated, e := translateCandidates(encodedWire, target, body, route)
			if e != nil {
				return c, e
			}
			if translated != nil {
				c.Body = translated
				return c, nil
			}
		}
		c.Body = renderCompletion(c, target, route)
	}
	return c, nil
}

func decodeWireCompletion(encodedWire, target wire.Format, body []byte, route, encoding string, request *wire.Request) (*wire.Completion, error) {
	switch encodedWire {
	case wire.Completions:
		return openai.DecodeCompletions(body, route)
	case wire.Chat, wire.MistralFIM:
		return openai.DecodeChat(body, route, target)
	case wire.Responses:
		return decodeResponsesWire(target, body, route, request)
	case wire.CohereChat:
		return decodeCohereChat(body)
	case wire.Anthropic:
		return decodeAnthropic(body, route)
	case wire.Gemini:
		return decodeGemini(body, route, false)
	case wire.InputTokens, wire.AnthropicCount, wire.GeminiCount, wire.BedrockCount:
		return decodeCountResult(encodedWire, target, body)
	case wire.Embeddings:
		return decodeEmbeddings(body, route, encoding, request)
	case wire.GeminiEmbeddings:
		return decodeGeminiEmbedding(body, route, encoding)
	case wire.GeminiEmbeddingsBatch:
		return decodeGeminiEmbeddingBatch(body, route, encoding, request)
	case wire.VertexEmbeddings:
		return decodeVertexEmbeddings(body, route, encoding, request)
	case wire.BedrockEmbeddings:
		return decodeBedrockEmbedding(body, route, encoding)
	case wire.Rerank:
		return decodeRerank(body, route, request)
	case wire.BedrockRerank:
		return decodeBedrockRerank(body, route, request)
	case wire.Moderation:
		return decodeModerationResult(body, route, request)
	case "bedrock":
		return decodeBedrock(body, route)
	default:
		return nil, protocolError("unknown response family")
	}
}

func decodeResponsesWire(target wire.Format, body []byte, route string, request *wire.Request) (*wire.Completion, error) {
	if target == wire.Anthropic || target == wire.Chat {
		response, parseErr := object(body)
		if parseErr != nil {
			return nil, parseErr
		}
		if validationErr := validateResponsesOutput(response); validationErr != nil {
			return nil, validationErr
		}
	}
	var background bool
	if request != nil && target == wire.Responses {
		_ = json.Unmarshal(request.Field("background"), &background)
	}
	if background {
		return openai.DecodeBackgroundResponse(body, route)
	}
	return openai.DecodeResponse(body, route)
}

func decodeCountResult(encodedWire, target wire.Format, body []byte) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid count result")
	}
	key := "input_tokens"
	if encodedWire == wire.GeminiCount {
		key = "totalTokens"
	}
	if encodedWire == wire.BedrockCount {
		key = "inputTokens"
	}
	n, ok := count(f[key])
	if !ok {
		return nil, protocolError("missing token count")
	}
	out := Object{"input_tokens": raw(n), "object": raw("response.input_tokens")}
	if target == wire.AnthropicCount {
		out = Object{"input_tokens": raw(n)}
	}
	if target == wire.GeminiCount {
		out = Object{"totalTokens": raw(n)}
	}
	if encodedWire == target {
		out = f
	}
	return &wire.Completion{Body: raw(out), FinishReason: "stop", Usage: &wire.Usage{InputTokens: n, TotalTokens: n}}, nil
}

func decodeModerationResult(body []byte, route string, request *wire.Request) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid moderation result")
	}
	if len(arr(f["results"])) == 0 {
		return nil, protocolError("missing moderation results")
	}
	if err := validateModerationCount(request, len(arr(f["results"]))); err != nil {
		return nil, err
	}
	for _, v := range arr(f["results"]) {
		r, e := object(v)
		if e != nil {
			return nil, protocolError("invalid moderation result")
		}
		var flagged bool
		if !present(r["flagged"]) || json.Unmarshal(r["flagged"], &flagged) != nil {
			return nil, protocolError("missing moderation decision")
		}
		categories, e := object(r["categories"])
		if e != nil || len(categories) == 0 {
			return nil, protocolError("missing moderation categories")
		}
		for _, v := range categories {
			var decision bool
			if !present(v) || json.Unmarshal(v, &decision) != nil {
				return nil, protocolError("invalid moderation category")
			}
		}
		scores, e := object(r["category_scores"])
		if e != nil || len(scores) != len(categories) {
			return nil, protocolError("missing moderation scores")
		}
		for category := range categories {
			v := scores[category]
			var score float64
			if !present(v) || json.Unmarshal(v, &score) != nil || math.IsInf(score, 0) || math.IsNaN(score) {
				return nil, protocolError("invalid moderation score")
			}
		}
	}
	if _, ok := f["model"]; ok {
		f["model"] = raw(route)
	}
	return &wire.Completion{Body: raw(f), FinishReason: "stop"}, nil
}

func validateResponsesOutput(response Object) error {
	for _, value := range arr(response["output"]) {
		item, err := object(value)
		if err != nil {
			return protocolError("invalid translated output item")
		}
		if err := validateResponsesItem(item); err != nil {
			return err
		}
	}
	return nil
}

func validateResponsesItem(item Object) error {
	switch str(item["type"]) {
	case "reasoning":
		for name, value := range item {
			switch name {
			case "type", "id", "status":
			case "summary":
				var summary []json.RawMessage
				if err := json.Unmarshal(value, &summary); err != nil || len(summary) != 0 {
					return protocolError("unrepresentable translated reasoning summary")
				}
			default:
				return protocolError("unrepresentable translated reasoning state")
			}
		}
		return nil
	case "function_call":
		return nil
	case "message":
		for _, value := range arr(item["content"]) {
			part, err := object(value)
			if err != nil || (str(part["type"]) != "output_text" && str(part["type"]) != "refusal") {
				return protocolError("unrepresentable translated response content")
			}
		}
		return nil
	default:
		return protocolError("unrepresentable translated output item")
	}
}

func decodeAnthropic(body []byte, route string) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("message is not an object")
	}
	if str(f["type"]) == "error" {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	if str(f["type"]) != "message" || str(f["role"]) != "assistant" {
		return nil, protocolError("invalid Anthropic message")
	}
	reason := str(f["stop_reason"])
	if reason == "" {
		return nil, protocolError("message has no stop reason")
	}
	c := &wire.Completion{UpstreamID: str(f["id"]), ProviderModel: str(f["model"]), FinishReason: anthropicFinish(reason)}
	if err := anthropicContent(c, f["content"]); err != nil {
		return nil, err
	}
	usage, e := optionalObject(f["usage"])
	if e != nil {
		return nil, e
	}
	c.Usage, e = nativeUsage(usage, "anthropic")
	if e != nil {
		return nil, e
	}
	f["model"] = raw(route)
	c.Body = raw(f)
	return c, nil
}

func optionalObject(v json.RawMessage) (Object, error) {
	if !present(v) {
		return nil, nil
	}
	f, e := object(v)
	if e != nil {
		return nil, protocolError("expected response object")
	}
	return f, nil
}

func anthropicContent(c *wire.Completion, v json.RawMessage) error {
	parts := arr(v)
	if parts == nil {
		return protocolError("message content is not an array")
	}
	for _, v := range parts {
		p, e := object(v)
		if e != nil {
			return protocolError("invalid message content")
		}
		switch str(p["type"]) {
		case "text":
			var text string
			if json.Unmarshal(p["text"], &text) != nil {
				return protocolError("invalid text block")
			}
			c.OutputText += text
		case "tool_use":
			if str(p["id"]) == "" || str(p["name"]) == "" {
				return protocolError("incomplete tool call")
			}
			if _, e := object(p["input"]); e != nil {
				return protocolError("invalid tool input")
			}
			c.ToolCalls = append(c.ToolCalls, wire.ToolCall{ID: str(p["id"]), Name: str(p["name"]), Arguments: string(p["input"])})
		}
	}
	return nil
}

func anthropicFinish(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func decodeGemini(body []byte, route string, partial bool) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("generation is not an object")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	candidates := arr(f["candidates"])
	if len(candidates) == 0 && !present(f["promptFeedback"]) && !partial {
		return nil, protocolError("generation has no candidates")
	}
	c := &wire.Completion{UpstreamID: str(f["responseId"]), ProviderModel: str(f["modelVersion"])}
	seen := map[int64]bool{}
	for i, v := range candidates {
		target := c
		if i > 0 {
			target = &wire.Completion{}
		}
		if err := decodeGeminiCandidate(v, i, partial, seen, target); err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 && present(f["promptFeedback"]) {
		c.FinishReason = "content_filter"
	}
	usage, e := optionalObject(f["usageMetadata"])
	if e != nil {
		return nil, e
	}
	c.Usage, e = nativeUsage(usage, "gemini")
	if e != nil {
		return nil, e
	}
	if e := geminiGrounding(c.Usage, f); e != nil {
		return nil, e
	}
	if _, ok := f["modelVersion"]; ok {
		f["modelVersion"] = raw(route)
	}
	c.Body = raw(f)
	return c, nil
}

func decodeGeminiCandidate(v json.RawMessage, i int, partial bool, seen map[int64]bool, c *wire.Completion) error {
	candidate, e := object(v)
	if e != nil {
		return protocolError("invalid candidate")
	}
	idx, ok := count(candidate["index"])
	if !ok {
		if present(candidate["index"]) {
			return protocolError("invalid candidate index")
		}
		idx = int64(i)
	}
	if seen[idx] {
		return protocolError("duplicate candidate index")
	}
	seen[idx] = true
	reason := str(candidate["finishReason"])
	if !partial && reason == "" {
		return protocolError("candidate has no finish reason")
	}
	content, e := optionalObject(candidate["content"])
	if e != nil {
		return e
	}
	for j, rawPart := range arr(content["parts"]) {
		if err := decodeGeminiCandidatePart(rawPart, j, c); err != nil {
			return err
		}
	}
	if reason != "" {
		c.FinishReason = geminiFinish(reason)
		if c.FinishReason == "stop" && len(c.ToolCalls) > 0 {
			c.FinishReason = "tool_calls"
		}
	}
	return nil
}

func decodeGeminiCandidatePart(v json.RawMessage, j int, c *wire.Completion) error {
	p, e := object(v)
	if e != nil {
		return protocolError("invalid candidate part")
	}
	if present(p["text"]) {
		var text string
		if json.Unmarshal(p["text"], &text) != nil {
			return protocolError("invalid candidate text")
		}
		var thought bool
		_ = json.Unmarshal(p["thought"], &thought)
		if !thought {
			c.OutputText += text
		}
	}
	if present(p["functionCall"]) {
		call, e := object(p["functionCall"])
		if e != nil {
			return protocolError("invalid function call")
		}
		id := str(call["id"])
		if id == "" {
			id = "call_" + strconv.Itoa(j)
		}
		if str(call["name"]) == "" {
			return protocolError("function call has no name")
		}
		if present(call["partialArgs"]) || present(call["willContinue"]) {
			return protocolError("partial function arguments are unsupported")
		}
		arguments := geminiArguments(call["args"])
		if _, e := object(arguments); e != nil {
			return protocolError("invalid function arguments")
		}
		c.ToolCalls = append(c.ToolCalls, wire.ToolCall{ID: id, Name: str(call["name"]), Arguments: string(arguments)})
	}
	return nil
}

func validateModerationCount(request *wire.Request, actual int) error {
	if request == nil || request.Family != wire.Moderation {
		return nil
	}
	input, exists := request.OIF().Document().Root().Lookup("input")
	if !exists {
		return nil
	}
	if actual != moderationResultCount(input) {
		return protocolError("moderation count does not match input scope")
	}
	return nil
}

func moderationResultCount(input oif.Value) int {
	if items := input.Elements(); input.Kind() == oif.Array && len(items) > 0 && items[0].Kind() == oif.String {
		return len(items)
	}
	return 1
}

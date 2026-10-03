//go:build integration

package gosdk

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// weatherParameters is the JSON schema of the weather tool's arguments.
var weatherParameters = map[string]any{
	"type":       "object",
	"properties": map[string]any{"city": map[string]any{"type": "string"}},
	"required":   []string{"city"},
}

var openAIWeather = openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
	Name: "get_weather", Description: openai.String("Current weather for a city."), Parameters: weatherParameters,
})

func TestOpenAIChatCompletion(t *testing.T) {
	h := connect(t)
	var response *http.Response
	completion, err := h.openAI().Chat.Completions.New(timeout(t), openai.ChatCompletionNewParams{
		Model:               h.models["openai"],
		Messages:            []openai.ChatCompletionMessageParamUnion{openai.SystemMessage("You are terse."), openai.UserMessage("Say hello.")},
		MaxCompletionTokens: openai.Int(64),
	}, openaioption.WithResponseInto(&response))
	if err != nil {
		t.Fatal(err)
	}
	choice := completion.Choices[0]
	if choice.Message.Content != h.defaultReply || choice.FinishReason != "stop" || completion.Model != h.models["openai"] {
		t.Fatalf("reply %q (%s) from model %q", choice.Message.Content, choice.FinishReason, completion.Model)
	}
	if completion.Usage.PromptTokens == 0 || completion.Usage.CompletionTokens == 0 {
		t.Fatalf("usage %+v", completion.Usage)
	}
	if response.Header.Get("X-Request-Id") == "" {
		t.Fatalf("the response carries no request ID: %v", response.Header)
	}

	r := h.onlyRequest(t)
	body := r.json(t)
	if r.Path != "/openai/v1/chat/completions" || r.Model != h.upstreamModels["openai"] || r.Stream {
		t.Fatalf("upstream saw %+v", r)
	}
	expect(t, body, h.upstreamModels["openai"], "model")
	expect(t, body, "system", "messages", 0, "role")
	expect(t, body, "Say hello.", "messages", 1, "content")
	expect(t, body, 64.0, "max_completion_tokens")
}

func TestOpenAIChatCompletionStreaming(t *testing.T) {
	h := connect(t)
	stream := h.openAI().Chat.Completions.NewStreaming(timeout(t), openai.ChatCompletionNewParams{
		Model:         h.models["openai"],
		Messages:      []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Say hello.")},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	})
	defer stream.Close()
	var acc openai.ChatCompletionAccumulator
	chunks := 0
	for stream.Next() {
		acc.AddChunk(stream.Current())
		chunks++
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if chunks < 3 {
		t.Fatalf("the text arrived in %d chunks", chunks)
	}
	choice := acc.Choices[0]
	if choice.Message.Content != h.defaultReply || choice.FinishReason != "stop" || acc.Model != h.models["openai"] {
		t.Fatalf("assembled %q (%s) from model %q", choice.Message.Content, choice.FinishReason, acc.Model)
	}
	if acc.Usage.TotalTokens == 0 {
		t.Fatalf("the usage chunk did not arrive: %+v", acc.Usage)
	}

	r := h.onlyRequest(t)
	body := r.json(t)
	if r.Path != "/openai/v1/chat/completions" || !r.Stream || r.Model != h.upstreamModels["openai"] {
		t.Fatalf("upstream saw %+v", r)
	}
	expect(t, body, true, "stream")
	expect(t, body, true, "stream_options", "include_usage")
}

func TestOpenAIChatToolLoop(t *testing.T) {
	h := connect(t)
	client := h.openAI()
	prompt := "Weather in Paris and Rome? " + tool("get_weather", map[string]any{"city": "Paris"}) + also("get_weather", map[string]any{"city": "Rome"})
	messages := []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)}
	params := func() openai.ChatCompletionNewParams {
		return openai.ChatCompletionNewParams{Model: h.models["openai"], Messages: messages, Tools: []openai.ChatCompletionToolUnionParam{openAIWeather}}
	}

	first, err := client.Chat.Completions.New(timeout(t), params())
	if err != nil {
		t.Fatal(err)
	}
	assistant := first.Choices[0]
	if assistant.FinishReason != "tool_calls" || len(assistant.Message.ToolCalls) != 2 {
		t.Fatalf("finish %q with %d tool calls", assistant.FinishReason, len(assistant.Message.ToolCalls))
	}
	cities := []string{"Paris", "Rome"}
	conditions := []string{"sunny", "rainy"}
	messages = append(messages, assistant.Message.ToParam())
	for i, call := range assistant.Message.ToolCalls {
		function := call.AsFunction()
		if function.Function.Name != "get_weather" || call.ID == "" {
			t.Fatalf("tool call %d: %+v", i, call)
		}
		var args struct{ City string }
		if err := json.Unmarshal([]byte(function.Function.Arguments), &args); err != nil || args.City != cities[i] {
			t.Fatalf("tool call %d arguments %q (%v)", i, function.Function.Arguments, err)
		}
		messages = append(messages, openai.ToolMessage(conditions[i], call.ID))
	}

	final, err := client.Chat.Completions.New(timeout(t), params())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := final.Choices[0].Message.Content, h.afterTools("sunny", "rainy"); got != want {
		t.Fatalf("final text %q, want %q", got, want)
	}

	requests := h.requests(t, 2)
	if requests[0].Script != "tool_call" || requests[1].Script != "tool_result" {
		t.Fatalf("scripts %q, %q", requests[0].Script, requests[1].Script)
	}
	follow := requests[1].json(t)
	for i, call := range assistant.Message.ToolCalls {
		expect(t, follow, call.ID, "messages", 1, "tool_calls", i, "id")
		expect(t, follow, "tool", "messages", 2+i, "role")
		expect(t, follow, call.ID, "messages", 2+i, "tool_call_id")
		expect(t, follow, conditions[i], "messages", 2+i, "content")
	}
	expect(t, follow, "get_weather", "tools", 0, "function", "name")
}

func TestOpenAIChatToolCallStreams(t *testing.T) {
	h := connect(t)
	prompt := "Weather in Paris? " + tool("get_weather", map[string]any{"city": "Paris"})
	stream := h.openAI().Chat.Completions.NewStreaming(timeout(t), openai.ChatCompletionNewParams{
		Model: h.models["openai"], Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
		Tools: []openai.ChatCompletionToolUnionParam{openAIWeather},
	})
	defer stream.Close()
	var acc openai.ChatCompletionAccumulator
	finished := 0
	for stream.Next() {
		acc.AddChunk(stream.Current())
		if call, ok := acc.JustFinishedToolCall(); ok {
			finished++
			if call.Name != "get_weather" || call.Arguments != `{"city":"Paris"}` {
				t.Fatalf("finished tool call %+v", call)
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if finished != 1 || acc.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("%d finished tool calls, finish reason %q", finished, acc.Choices[0].FinishReason)
	}

	// The SDK's own assembly is what a loop sends back.
	assistant := acc.Choices[0].Message
	messages := []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt), assistant.ToParam(), openai.ToolMessage("sunny", assistant.ToolCalls[0].ID)}
	final, err := h.openAI().Chat.Completions.New(timeout(t), openai.ChatCompletionNewParams{
		Model: h.models["openai"], Messages: messages, Tools: []openai.ChatCompletionToolUnionParam{openAIWeather},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := final.Choices[0].Message.Content, h.afterTools("sunny"); got != want {
		t.Fatalf("final text %q, want %q", got, want)
	}
	requests := h.requests(t, 2)
	if !requests[0].Stream || requests[1].Stream || requests[1].Script != "tool_result" {
		t.Fatalf("requests %+v", requests)
	}
	expect(t, requests[1].json(t), assistant.ToolCalls[0].ID, "messages", 2, "tool_call_id")
}

func TestOpenAIChatStructuredOutput(t *testing.T) {
	h := connect(t)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city":  map[string]any{"type": "string"},
			"temp":  map[string]any{"type": "integer"},
			"rainy": map[string]any{"type": "boolean"},
		},
		"required":             []string{"city", "temp", "rainy"},
		"additionalProperties": false,
	}
	completion, err := h.openAI().Chat.Completions.New(timeout(t), openai.ChatCompletionNewParams{
		Model:    h.models["openai"],
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Forecast for Paris.")},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{Name: "forecast", Strict: openai.Bool(true), Schema: schema},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var forecast struct {
		City  string `json:"city"`
		Temp  int    `json:"temp"`
		Rainy bool   `json:"rainy"`
	}
	decoder := json.NewDecoder(strings.NewReader(completion.Choices[0].Message.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&forecast); err != nil {
		t.Fatalf("the reply %q is not an instance of the schema: %v", completion.Choices[0].Message.Content, err)
	}

	body := h.onlyRequest(t).json(t)
	expect(t, body, "json_schema", "response_format", "type")
	expect(t, body, "forecast", "response_format", "json_schema", "name")
	expect(t, body, "integer", "response_format", "json_schema", "schema", "properties", "temp", "type")
}

func TestOpenAIResponses(t *testing.T) {
	h := connect(t)
	client := h.openAI()
	// A stateless exchange, as a client does that must not retain provider state:
	// the reasoning item travels back with its encrypted content.
	prompt := "Weather in Paris? " + tool("get_weather", map[string]any{"city": "Paris"})
	tools := []responses.ToolUnionParam{responses.ToolParamOfFunction("get_weather", weatherParameters, false)}
	options := func(input responses.ResponseNewParamsInputUnion) responses.ResponseNewParams {
		return responses.ResponseNewParams{
			Model: h.models["openai"], Input: input, Tools: tools, Store: openai.Bool(false),
			Reasoning: shared.ReasoningParam{Effort: shared.ReasoningEffortLow, Summary: shared.ReasoningSummaryAuto},
			Include:   []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		}
	}

	first, err := client.Responses.New(timeout(t), options(responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)}))
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, item := range first.Output {
		types = append(types, item.Type)
	}
	if !slices.Equal(types, []string{"reasoning", "function_call"}) {
		t.Fatalf("output items %v", types)
	}
	reasoning, call := first.Output[0].AsReasoning(), first.Output[1].AsFunctionCall()
	if reasoning.EncryptedContent == "" || call.Name != "get_weather" || call.Arguments != `{"city":"Paris"}` || call.CallID == "" {
		t.Fatalf("reasoning %+v, call %+v", reasoning, call)
	}

	input := responses.ResponseInputParam{
		responses.ResponseInputItemParamOfMessage(prompt, responses.EasyInputMessageRoleUser),
		{OfReasoning: new(reasoning.ToParam())},
		{OfFunctionCall: new(call.ToParam())},
		{OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: openai.String(call.CallID),
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: openai.String("sunny")},
		}},
	}
	final, err := client.Responses.New(timeout(t), options(responses.ResponseNewParamsInputUnion{OfInputItemList: input}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := final.OutputText(), h.afterTools("sunny"); got != want || final.Model != h.models["openai"] {
		t.Fatalf("final text %q from %q, want %q", got, final.Model, want)
	}
	if final.Usage.InputTokens == 0 || final.Usage.OutputTokens == 0 {
		t.Fatalf("usage %+v", final.Usage)
	}

	requests := h.requests(t, 2)
	for _, r := range requests {
		if r.Path != "/openai/v1/responses" || r.Model != h.upstreamModels["openai"] {
			t.Fatalf("upstream saw %+v", r)
		}
		expect(t, r.json(t), false, "store")
		expect(t, r.json(t), "reasoning.encrypted_content", "include", 0)
	}
	follow := requests[1].json(t)
	expect(t, follow, "reasoning", "input", 1, "type")
	expect(t, follow, reasoning.EncryptedContent, "input", 1, "encrypted_content")
	expect(t, follow, "function_call", "input", 2, "type")
	expect(t, follow, call.CallID, "input", 2, "call_id")
	expect(t, follow, "function_call_output", "input", 3, "type")
	expect(t, follow, call.CallID, "input", 3, "call_id")
	expect(t, follow, "sunny", "input", 3, "output")
}

func TestOpenAIResponsesWithoutStore(t *testing.T) {
	h := connect(t)
	input := responses.ResponseNewParamsInputUnion{OfString: openai.String("Say hello.")}
	// A transformed route serves the call as the SDK sends it, with no store.
	response, err := h.openAI().Responses.New(timeout(t), responses.ResponseNewParams{Model: h.models["openai"], Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputText() != h.defaultReply || response.Model != h.models["openai"] {
		t.Fatalf("reply %q from %q", response.OutputText(), response.Model)
	}
	r := h.onlyRequest(t)
	if r.Path != "/openai/v1/responses" || r.Model != h.upstreamModels["openai"] {
		t.Fatalf("upstream saw %+v", r)
	}

	// A strict route preserves the call, and the API reads an omitted store as
	// true, which retains provider state. Only a key that allows it may.
	_, err = h.openAI().Responses.New(timeout(t), responses.ResponseNewParams{Model: h.models["openai_strict"], Input: input})
	e, ok := errors.AsType[*openai.Error](err)
	if !ok || e.StatusCode != http.StatusBadRequest || e.Code != "policy_conflict" || e.Param != "store" {
		t.Fatalf("a strict route without store: %T %v", err, err)
	}
	if requests := h.recorded(t, nil); len(requests) != 1 {
		t.Fatalf("the refused call reached the upstream: %+v", requests)
	}
}

func TestOpenAIResponsesStreaming(t *testing.T) {
	h := connect(t)
	stream := h.openAI().Responses.NewStreaming(timeout(t), responses.ResponseNewParams{
		Model: h.models["openai"], Store: openai.Bool(false),
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("Say hello.")},
	})
	defer stream.Close()
	var text strings.Builder
	var completed *responses.Response
	deltas := 0
	for stream.Next() {
		switch event := stream.Current().AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			text.WriteString(event.Delta)
			deltas++
		case responses.ResponseCompletedEvent:
			completed = &event.Response
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if deltas < 2 || text.String() != h.defaultReply {
		t.Fatalf("%d deltas assembled %q", deltas, text.String())
	}
	if completed == nil || completed.OutputText() != h.defaultReply || completed.Usage.TotalTokens == 0 {
		t.Fatalf("completed response %+v", completed)
	}

	r := h.onlyRequest(t)
	if r.Path != "/openai/v1/responses" || !r.Stream || r.Model != h.upstreamModels["openai"] {
		t.Fatalf("upstream saw %+v", r)
	}
	expect(t, r.json(t), true, "stream")
}

func TestOpenAIResponsesFunctionCallStreams(t *testing.T) {
	h := connect(t)
	prompt := "Weather in Paris? " + tool("get_weather", map[string]any{"city": "Paris"})
	stream := h.openAI().Responses.NewStreaming(timeout(t), responses.ResponseNewParams{
		Model: h.models["openai"], Store: openai.Bool(false),
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)},
		Tools: []responses.ToolUnionParam{responses.ToolParamOfFunction("get_weather", weatherParameters, false)},
	})
	defer stream.Close()
	var arguments strings.Builder
	var done *responses.ResponseFunctionToolCall
	for stream.Next() {
		switch event := stream.Current().AsAny().(type) {
		case responses.ResponseFunctionCallArgumentsDeltaEvent:
			arguments.WriteString(event.Delta)
		case responses.ResponseOutputItemDoneEvent:
			if event.Item.Type == "function_call" {
				call := event.Item.AsFunctionCall()
				done = &call
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if arguments.String() != `{"city":"Paris"}` || done == nil || done.Name != "get_weather" || done.Arguments != `{"city":"Paris"}` {
		t.Fatalf("streamed arguments %q, finished call %+v", arguments.String(), done)
	}
	if r := h.onlyRequest(t); !r.Stream || r.Script != "tool_call" {
		t.Fatalf("upstream saw %+v", r)
	}
}

func TestOpenAIEmbeddings(t *testing.T) {
	h := connect(t)
	client := h.openAI()
	floats, err := client.Embeddings.New(timeout(t), openai.EmbeddingNewParams{
		Model: h.models["openai"], Dimensions: openai.Int(8),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{"alpha", "beta"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(floats.Data) != 2 || floats.Model != h.models["openai"] || floats.Usage.PromptTokens == 0 {
		t.Fatalf("embeddings %+v", floats)
	}
	for i, item := range floats.Data {
		if item.Index != int64(i) || len(item.Embedding) != 8 {
			t.Fatalf("embedding %d: %+v", i, item)
		}
		var norm float64
		for _, v := range item.Embedding {
			norm += v * v
		}
		if math.Abs(norm-1) > 1e-6 {
			t.Fatalf("embedding %d is not a unit vector: norm %v", i, norm)
		}
	}
	if slices.Equal(floats.Data[0].Embedding, floats.Data[1].Embedding) {
		t.Fatal("different inputs gave the same embedding")
	}

	// The same text embeds to the same vector, in either encoding.
	encoded, err := client.Embeddings.New(timeout(t), openai.EmbeddingNewParams{
		Model: h.models["openai"], Dimensions: openai.Int(8), EncodingFormat: openai.EmbeddingNewParamsEncodingFormatBase64,
		Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String("alpha")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Data []struct {
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(encoded.RawJSON()), &raw); err != nil || len(raw.Data) != 1 {
		t.Fatalf("base64 response %s: %v", encoded.RawJSON(), err)
	}
	bytes, err := base64.StdEncoding.DecodeString(raw.Data[0].Embedding)
	if err != nil || len(bytes) != 8*4 {
		t.Fatalf("base64 embedding %q: %d bytes, %v", raw.Data[0].Embedding, len(bytes), err)
	}
	for i, want := range floats.Data[0].Embedding {
		got := math.Float32frombits(binary.LittleEndian.Uint32(bytes[4*i:]))
		if math.Abs(float64(got)-want) > 1e-6 {
			t.Fatalf("component %d is %v in base64 and %v in floats", i, got, want)
		}
	}

	requests := h.requests(t, 2)
	for _, r := range requests {
		if r.Path != "/openai/v1/embeddings" || r.Model != h.upstreamModels["openai"] {
			t.Fatalf("upstream saw %+v", r)
		}
	}
	expect(t, requests[0].json(t), "beta", "input", 1)
	expect(t, requests[0].json(t), 8.0, "dimensions")
	expect(t, requests[1].json(t), "base64", "encoding_format")
}

func TestOpenAIModels(t *testing.T) {
	h := connect(t)
	client := h.openAI()
	page, err := client.Models.List(timeout(t))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, model := range page.Data {
		listed[model.ID] = true
	}
	for name, slug := range h.models {
		if !listed[slug] {
			t.Errorf("route %s (%s) is not listed: %v", name, slug, listed)
		}
	}
	model, err := client.Models.Get(timeout(t), h.models["openai"])
	if err != nil || model.ID != h.models["openai"] {
		t.Fatalf("model %+v: %v", model, err)
	}
	h.untouched(t)
}

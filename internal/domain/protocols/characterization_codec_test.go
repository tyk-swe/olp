package protocols_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

const toolSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

type requestFixture struct {
	format   wire.Format
	scenario string
	body     string
	upstream string
	targets  []wire.Format
}

func (f requestFixture) name() string {
	if f.scenario == "" {
		return string(f.format)
	}
	return string(f.format) + " (" + f.scenario + ")"
}

const (
	geminiRequest = `{"systemInstruction":{"parts":[{"text":"Be brief"}]},"contents":[{"role":"user","parts":[{"text":"Weather in Oslo?"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","description":"Look up weather","parameters":` + toolSchema + `}]}],"generationConfig":{"maxOutputTokens":64,"temperature":0.5}}`

	embeddingsRequest       = `{"model":"fixture-route","input":["first","second"]}`
	singleEmbeddingsRequest = `{"model":"fixture-route","input":"first"}`
	rerankRequest           = `{"model":"fixture-route","query":"weather","documents":["sunny","rain"],"top_n":2}`
	moderationRequest       = `{"model":"fixture-route","input":"some text"}`

	titanModel = "amazon.titan-embed-text-fixture"
)

var requestFixtures = []requestFixture{
	{wire.Chat, "", `{"model":"fixture-route","messages":[{"role":"system","content":"Be brief"},{"role":"user","content":"Weather in Oslo?"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Look up weather","parameters":` + toolSchema + `}}],"max_tokens":64,"temperature":0.5}`, upstreamModel, nil},
	{wire.Completions, "", `{"model":"fixture-route","prompt":"Say hello","max_tokens":16}`, upstreamModel, nil},
	{wire.Responses, "", `{"model":"fixture-route","instructions":"Be brief","input":"Weather in Oslo?","tools":[{"type":"function","name":"lookup","description":"Look up weather","parameters":` + toolSchema + `}],"max_output_tokens":64,"temperature":0.5}`, upstreamModel, nil},
	{wire.Anthropic, "", `{"model":"fixture-route","system":"Be brief","messages":[{"role":"user","content":"Weather in Oslo?"}],"tools":[{"name":"lookup","description":"Look up weather","input_schema":` + toolSchema + `}],"max_tokens":64,"temperature":0.5}`, upstreamModel, nil},
	{wire.Gemini, "", geminiRequest, upstreamModel, nil},
	{wire.GeminiStream, "", geminiRequest, upstreamModel, nil},
	{wire.Bedrock, "", `{"system":[{"text":"Be brief"}],"messages":[{"role":"user","content":[{"text":"Weather in Oslo?"}]}],"toolConfig":{"tools":[{"toolSpec":{"name":"lookup","description":"Look up weather","inputSchema":{"json":` + toolSchema + `}}}]},"inferenceConfig":{"maxTokens":64,"temperature":0.5}}`, upstreamModel, nil},
	{wire.MistralFIM, "", `{"prompt":"def f():"}`, upstreamModel, nil},
	{wire.MistralFIM, "every field", `{"prompt":"def f():","suffix":"return 1","max_tokens":16}`, upstreamModel, []wire.Format{wire.MistralFIM}},
	{wire.CohereChat, "", `{"messages":[{"role":"user","content":"Weather in Oslo?"}]}`, upstreamModel, nil},
	{wire.InputTokens, "", `{"model":"fixture-route","input":"Weather in Oslo?"}`, upstreamModel, nil},
	{wire.AnthropicCount, "", `{"model":"fixture-route","system":"Be brief","messages":[{"role":"user","content":"Weather in Oslo?"}]}`, upstreamModel, nil},
	{wire.GeminiCount, "", `{"contents":[{"role":"user","parts":[{"text":"Weather in Oslo?"}]}]}`, upstreamModel, nil},
	{wire.Embeddings, "", embeddingsRequest, upstreamModel, nil},
	{wire.Embeddings, "one input", singleEmbeddingsRequest, titanModel, nil},
	{wire.Rerank, "", rerankRequest, upstreamModel, nil},
	{wire.Moderation, "", moderationRequest, upstreamModel, nil},
}

func parseFixture(format wire.Format, body string) (*wire.Request, error) {
	if format == wire.Bedrock {
		return protocols.ParseBedrockRequest([]byte(body), routeModel, false)
	}
	return protocols.Parse(format, []byte(body), routeModel)
}

type parsedRequest struct {
	Error            *failure           `json:"error,omitempty"`
	Family           wire.Format        `json:"family,omitempty"`
	Route            string             `json:"route,omitempty"`
	Parameters       []string           `json:"parameters,omitempty"`
	Features         protocols.Features `json:"features"`
	Text             string             `json:"text,omitempty"`
	StructuredOutput bool               `json:"structured_output,omitempty"`
}

type encoded struct {
	Wire  wire.Format `json:"wire,omitempty"`
	Body  any         `json:"body,omitempty"`
	Error *failure    `json:"error,omitempty"`
}

func (f requestFixture) encode(target wire.Format) ([]byte, wire.Format, error) {
	request, err := parseFixture(f.format, f.body)
	if err != nil {
		return nil, target, fmt.Errorf("the %s fixture no longer parses: %w", f.format, err)
	}
	return protocols.EncodeTarget(request, target, "", "", f.upstream, nil)
}

func (f requestFixture) outcome(target wire.Format) (any, bool) {
	body, format, err := f.encode(target)
	if err != nil {
		return encoded{Error: classify(err)}, false
	}
	return encoded{Wire: format, Body: document(body)}, true
}

func (f requestFixture) reachable(t *testing.T) []wire.Format {
	t.Helper()
	if f.targets != nil {
		return f.targets
	}
	return formats(t)
}

func sameOperation(source wire.Format, targets []wire.Format) []wire.Format {
	return slices.DeleteFunc(slices.Clone(targets), func(target wire.Format) bool {
		return target.Operation() != source.Operation()
	})
}

func TestRequestCodecMatrix(t *testing.T) {
	t.Parallel()
	var cases []goldenCase
	for _, fixture := range requestFixtures {
		cases = append(cases, goldenCase{name: fixture.name() + " parsed", run: func(*testing.T) any {
			request, err := parseFixture(fixture.format, fixture.body)
			if err != nil {
				return parsedRequest{Error: classify(err)}
			}
			return parsedRequest{Family: request.Family, Route: request.Route, Parameters: slices.Sorted(slices.Values(protocols.ParameterNames(request))), Features: protocols.RequestFeatures(request), Text: protocols.RequestText(request, 1024), StructuredOutput: protocols.StructuredOutputRequested(request)}
		}})
		rows := newMatrix(preferred(fixture.format, sameOperation(fixture.format, fixture.reachable(t))), fixture.outcome)
		for _, target := range rows.stored {
			cases = append(cases, goldenCase{name: fixture.name() + " to " + string(target), run: func(*testing.T) any {
				return rows.row(target)
			}})
		}
	}
	checkGolden(t, "testdata/request_codec.golden.json", cases)
}

func TestRequestsAreRefusedAcrossOperations(t *testing.T) {
	t.Parallel()
	for _, fixture := range requestFixtures {
		for _, target := range fixture.reachable(t) {
			if target.Operation() == fixture.format.Operation() {
				continue
			}
			_, _, err := fixture.encode(target)
			var refusal *wire.RequestError
			if !errors.As(err, &refusal) || refusal.Code != "unsupported_parameter" || refusal.Param != "operation" {
				t.Errorf("%s (%s) to %s (%s) = %v, want the unsupported operation refusal", fixture.name(), fixture.format.Operation(), target, target.Operation(), err)
			}
		}
	}
}

type responseFixture struct {
	format   wire.Format
	scenario string
	body     string
	request  *requestFixture
}

var (
	embeddingsCaller       = &requestFixture{format: wire.Embeddings, body: embeddingsRequest}
	singleEmbeddingsCaller = &requestFixture{format: wire.Embeddings, body: singleEmbeddingsRequest}
	rerankCaller           = &requestFixture{format: wire.Rerank, body: rerankRequest}
	moderationCaller       = &requestFixture{format: wire.Moderation, body: moderationRequest}
)

var responseFixtures = []responseFixture{
	{wire.Chat, "tool call", `{"id":"chatcmpl-fixture","object":"chat.completion","created":1,"model":"fixture-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"Hello","tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Oslo\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`, nil},
	{wire.Chat, "refusal", `{"id":"chatcmpl-fixture","object":"chat.completion","created":1,"model":"fixture-upstream","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"I cannot help"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`, nil},
	{wire.Chat, "text", `{"id":"chatcmpl-fixture","object":"chat.completion","created":1,"model":"fixture-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`, nil},
	{wire.Chat, "length stop", `{"id":"chatcmpl-fixture","object":"chat.completion","created":1,"model":"fixture-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"Hel"},"finish_reason":"length"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`, nil},
	{wire.Completions, "text", `{"id":"cmpl-fixture","object":"text_completion","created":1,"model":"fixture-upstream","choices":[{"index":0,"text":"Hello","finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`, nil},
	{wire.Responses, "tool call", `{"id":"resp_fixture","object":"response","created_at":1,"model":"fixture-upstream","status":"completed","output":[{"id":"rs_fixture","type":"reasoning","summary":[]},{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]},{"id":"fc_fixture","type":"function_call","status":"completed","call_id":"call_fixture","name":"lookup","arguments":"{\"city\":\"Oslo\"}"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`, nil},
	{wire.Responses, "refusal", `{"id":"resp_fixture","object":"response","created_at":1,"model":"fixture-upstream","status":"completed","output":[{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I cannot help"}]}],"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}`, nil},
	{wire.Anthropic, "tool call", `{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture-upstream","content":[{"type":"thinking","thinking":"plan","signature":"c2lnbmVk"},{"type":"text","text":"Hello"},{"type":"tool_use","id":"toolu_fixture","name":"lookup","input":{"city":"Oslo"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3}}`, nil},
	{wire.Anthropic, "refusal", `{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture-upstream","content":[{"type":"text","text":"I cannot help"}],"stop_reason":"refusal","stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":4}}`, nil},
	{wire.Anthropic, "length stop", `{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture-upstream","content":[{"type":"text","text":"Hel"}],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":2}}`, nil},
	{wire.Gemini, "tool call", `{"responseId":"gemini-fixture","modelVersion":"fixture-upstream","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"plan","thought":true},{"text":"Hello"},{"functionCall":{"name":"lookup","args":{"city":"Oslo"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":18,"cachedContentTokenCount":3}}`, nil},
	{wire.Gemini, "safety stop", `{"responseId":"gemini-fixture","modelVersion":"fixture-upstream","candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":0,"totalTokenCount":11}}`, nil},
	{wire.Gemini, "blocked prompt", `{"responseId":"gemini-fixture","modelVersion":"fixture-upstream","promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":11,"totalTokenCount":11}}`, nil},
	{wire.Gemini, "length stop", `{"responseId":"gemini-fixture","modelVersion":"fixture-upstream","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hel"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":2,"totalTokenCount":13}}`, nil},
	{wire.GeminiStream, "tool call", `{"responseId":"gemini-fixture","modelVersion":"fixture-upstream","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"},{"functionCall":{"name":"lookup","args":{"city":"Oslo"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7,"totalTokenCount":18}}`, nil},
	{wire.Bedrock, "tool call", `{"output":{"message":{"role":"assistant","content":[{"reasoningContent":{"reasoningText":{"text":"plan","signature":"c2lnbmVk"}}},{"text":"Hello"},{"toolUse":{"toolUseId":"tooluse_fixture","name":"lookup","input":{"city":"Oslo"}}}]}},"stopReason":"tool_use","usage":{"inputTokens":11,"outputTokens":7,"totalTokens":18,"cacheReadInputTokens":3},"metrics":{"latencyMs":5}}`, nil},
	{wire.Bedrock, "guardrail stop", `{"output":{"message":{"role":"assistant","content":[{"text":"I cannot help"}]}},"stopReason":"guardrail_intervened","usage":{"inputTokens":11,"outputTokens":4,"totalTokens":15},"metrics":{"latencyMs":5}}`, nil},
	{wire.Bedrock, "length stop", `{"output":{"message":{"role":"assistant","content":[{"text":"Hel"}]}},"stopReason":"max_tokens","usage":{"inputTokens":11,"outputTokens":2,"totalTokens":13},"metrics":{"latencyMs":5}}`, nil},
	{wire.MistralFIM, "text", `{"id":"fim-fixture","object":"chat.completion","created":1,"model":"fixture-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`, nil},
	{wire.CohereChat, "tool call", `{"id":"cohere-fixture","finish_reason":"TOOL_CALL","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}],"tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Oslo\"}"}}]},"usage":{"billed_units":{"input_tokens":11,"output_tokens":7},"tokens":{"input_tokens":13,"output_tokens":7}}}`, nil},
	{wire.InputTokens, "count", `{"object":"response.input_tokens","input_tokens":11}`, nil},
	{wire.AnthropicCount, "count", `{"input_tokens":11}`, nil},
	{wire.GeminiCount, "count", `{"totalTokens":11}`, nil},
	{wire.BedrockCount, "count", `{"inputTokens":11}`, nil},
	{wire.Embeddings, "vectors", `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.5,0.25]},{"object":"embedding","index":1,"embedding":[0.125,1]}],"model":"fixture-upstream","usage":{"prompt_tokens":4,"total_tokens":4}}`, embeddingsCaller},
	{wire.GeminiEmbeddings, "vectors", `{"embedding":{"values":[0.5,0.25]},"usageMetadata":{"promptTokenCount":2,"totalTokenCount":2}}`, singleEmbeddingsCaller},
	{wire.GeminiEmbeddingsBatch, "vectors", `{"embeddings":[{"values":[0.5,0.25]},{"values":[0.125,1]}],"usageMetadata":{"promptTokenCount":4,"totalTokenCount":4}}`, embeddingsCaller},
	{wire.VertexEmbeddings, "vectors", `{"predictions":[{"embeddings":{"values":[0.5,0.25],"statistics":{"token_count":2,"truncated":false}}},{"embeddings":{"values":[0.125,1],"statistics":{"token_count":2,"truncated":false}}}]}`, embeddingsCaller},
	{wire.BedrockEmbeddings, "vectors", `{"embedding":[0.5,0.25],"inputTextTokenCount":2}`, singleEmbeddingsCaller},
	{wire.BedrockEmbeddings, "base64 vectors", `{"embedding":[0.5,0.25],"inputTextTokenCount":2}`, &requestFixture{format: wire.Embeddings, body: `{"model":"fixture-route","input":"first","encoding_format":"base64"}`}},
	{wire.Rerank, "ranking", `{"id":"rerank-fixture","model":"fixture-upstream","results":[{"index":1,"relevance_score":0.75},{"index":0,"relevance_score":0.25}],"usage":{"total_tokens":6}}`, rerankCaller},
	{wire.BedrockRerank, "ranking", `{"results":[{"index":1,"relevanceScore":0.75},{"index":0,"relevanceScore":0.25}]}`, rerankCaller},
	{wire.Moderation, "flagged", `{"id":"modr-fixture","model":"fixture-upstream","results":[{"flagged":true,"categories":{"hate":false,"violence":true},"category_scores":{"hate":0.125,"violence":0.75}}]}`, moderationCaller},
}

func (f responseFixture) decode(target wire.Format) (*wire.Completion, error) {
	if f.request == nil {
		return protocols.DecodeRequest(f.format, target, []byte(f.body), routeModel, "", nil)
	}
	caller, err := parseFixture(f.request.format, f.request.body)
	if err != nil {
		return nil, fmt.Errorf("the %s caller fixture no longer parses: %w", f.request.format, err)
	}
	return protocols.DecodeRequest(f.format, target, []byte(f.body), routeModel, protocols.EmbeddingEncoding(caller, nil), caller)
}

func (f responseFixture) outcome(target wire.Format) (any, bool) {
	completion, err := f.decode(target)
	return unaryOutcome(completion, err), err == nil
}

func (f responseFixture) name(target string) string {
	return string(f.format) + " to " + target + ", " + f.scenario
}

func (f responseFixture) projects(target wire.Format) bool {
	return target.Operation() == wire.OperationGeneration || target.Operation() == f.format.Operation()
}

func TestResponseCodecMatrix(t *testing.T) {
	t.Parallel()
	var cases []goldenCase
	for _, fixture := range responseFixtures {
		all := formats(t)
		others := slices.DeleteFunc(slices.Clone(all), fixture.projects)
		rows := newMatrix(preferred(fixture.format, slices.DeleteFunc(slices.Clone(all), func(target wire.Format) bool { return !fixture.projects(target) })), fixture.outcome)
		for _, target := range rows.stored {
			cases = append(cases, goldenCase{name: fixture.name(string(target)), run: func(*testing.T) any {
				return rows.row(target)
			}})
		}
		cases = append(cases, goldenCase{name: fixture.name("any other operation"), run: func(t *testing.T) any {
			first := rows.row(others[0])
			for _, target := range others[1:] {
				if other := rows.row(target); !reflect.DeepEqual(first, other) {
					t.Errorf("decoding to %s gives %+v, while decoding to %s gives %+v", target, other, others[0], first)
				}
			}
			return first
		}})
	}
	checkGolden(t, "testdata/response_codec.golden.json", cases)
}

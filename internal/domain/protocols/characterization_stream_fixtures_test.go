package protocols_test

import (
	"bytes"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type streamScenario struct {
	name string
	body []byte
}

type streamSource struct {
	format    wire.Format
	scenarios []streamScenario
}

func dataFrames(documents ...string) []byte {
	return []byte("data: " + strings.Join(documents, "\n\ndata: ") + "\n\n")
}

type namedEvent struct{ name, document string }

func namedFrames(events ...namedEvent) []byte {
	var out strings.Builder
	for _, event := range events {
		out.WriteString("event: " + event.name + "\ndata: " + event.document + "\n\n")
	}
	return []byte(out.String())
}

func bedrockMessage(messageType, kind, payload string) []byte {
	headers := eventstream.Headers{}
	headers.Set(":message-type", eventstream.StringValue(messageType))
	headers.Set(":event-type", eventstream.StringValue(kind))
	if messageType == "exception" {
		headers.Set(":exception-type", eventstream.StringValue(kind))
	}
	var out bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&out, eventstream.Message{Headers: headers, Payload: []byte(payload)}); err != nil {
		panic(err)
	}
	return out.Bytes()
}

func bedrockFrames(events ...namedEvent) []byte {
	var out bytes.Buffer
	for _, event := range events {
		out.Write(bedrockMessage("event", event.name, event.document))
	}
	return out.Bytes()
}

const chatChunk = `{"id":"chatcmpl-fixture","object":"chat.completion.chunk","created":1,"model":"fixture-upstream","choices":[{"index":0,"delta":`

var chatToolStream = dataFrames(
	chatChunk+`{"role":"assistant","content":""},"finish_reason":null}]}`,
	chatChunk+`{"content":"Hel"},"finish_reason":null}]}`,
	chatChunk+`{"content":"lo"},"finish_reason":null}]}`,
	chatChunk+`{"tool_calls":[{"index":0,"id":"call_fixture","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
	chatChunk+`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
	chatChunk+`{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]},"finish_reason":null}]}`,
	chatChunk+`{},"finish_reason":"tool_calls"}]}`,
	`{"id":"chatcmpl-fixture","object":"chat.completion.chunk","created":1,"model":"fixture-upstream","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
	`[DONE]`,
)

var chatRefusalStream = dataFrames(
	chatChunk+`{"role":"assistant","content":""},"finish_reason":null}]}`,
	chatChunk+`{"refusal":"I cannot "},"finish_reason":null}]}`,
	chatChunk+`{"refusal":"help"},"finish_reason":null}]}`,
	chatChunk+`{},"finish_reason":"stop"}]}`,
	`{"id":"chatcmpl-fixture","object":"chat.completion.chunk","created":1,"model":"fixture-upstream","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`,
	`[DONE]`,
)

var chatTextStream = dataFrames(
	chatChunk+`{"role":"assistant","content":""},"finish_reason":null}]}`,
	chatChunk+`{"content":"Hel"},"finish_reason":null}]}`,
	chatChunk+`{"content":"lo"},"finish_reason":null}]}`,
	chatChunk+`{},"finish_reason":"stop"}]}`,
	`{"id":"chatcmpl-fixture","object":"chat.completion.chunk","created":1,"model":"fixture-upstream","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
	`[DONE]`,
)

const completionChunk = `{"id":"cmpl-fixture","object":"text_completion","created":1,"model":"fixture-upstream","choices":[{"index":0,"text":`

var completionsStream = dataFrames(
	completionChunk+`"Hel","finish_reason":null}]}`,
	completionChunk+`"lo","finish_reason":null}]}`,
	completionChunk+`"","finish_reason":"stop"}]}`,
	`{"id":"cmpl-fixture","object":"text_completion","created":1,"model":"fixture-upstream","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
	`[DONE]`,
)

const responseShell = `"id":"resp_fixture","object":"response","created_at":1,"model":"fixture-upstream"`

var responsesToolStream = namedFrames(
	namedEvent{"response.created", `{"type":"response.created","sequence_number":0,"response":{` + responseShell + `,"status":"in_progress","output":[]}}`},
	namedEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"rs_fixture","type":"reasoning","summary":[]}}`},
	namedEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"id":"rs_fixture","type":"reasoning","summary":[]}}`},
	namedEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"id":"msg_fixture","type":"message","role":"assistant","status":"in_progress","content":[]}}`},
	namedEvent{"response.content_part.added", `{"type":"response.content_part.added","sequence_number":4,"item_id":"msg_fixture","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`},
	namedEvent{"response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":5,"item_id":"msg_fixture","output_index":1,"content_index":0,"delta":"Hel"}`},
	namedEvent{"response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":6,"item_id":"msg_fixture","output_index":1,"content_index":0,"delta":"lo"}`},
	namedEvent{"response.output_text.done", `{"type":"response.output_text.done","sequence_number":7,"item_id":"msg_fixture","output_index":1,"content_index":0,"text":"Hello"}`},
	namedEvent{"response.content_part.done", `{"type":"response.content_part.done","sequence_number":8,"item_id":"msg_fixture","output_index":1,"content_index":0,"part":{"type":"output_text","text":"Hello","annotations":[]}}`},
	namedEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":9,"output_index":1,"item":{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]}}`},
	namedEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":10,"output_index":2,"item":{"id":"fc_fixture","type":"function_call","status":"in_progress","call_id":"call_fixture","name":"lookup","arguments":""}}`},
	namedEvent{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":11,"item_id":"fc_fixture","output_index":2,"delta":"{\"city\":"}`},
	namedEvent{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":12,"item_id":"fc_fixture","output_index":2,"delta":"\"Oslo\"}"}`},
	namedEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":13,"item_id":"fc_fixture","output_index":2,"arguments":"{\"city\":\"Oslo\"}"}`},
	namedEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":14,"output_index":2,"item":{"id":"fc_fixture","type":"function_call","status":"completed","call_id":"call_fixture","name":"lookup","arguments":"{\"city\":\"Oslo\"}"}}`},
	namedEvent{"response.completed", `{"type":"response.completed","sequence_number":15,"response":{` + responseShell + `,"status":"completed","output":[{"id":"rs_fixture","type":"reasoning","summary":[]},{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]},{"id":"fc_fixture","type":"function_call","status":"completed","call_id":"call_fixture","name":"lookup","arguments":"{\"city\":\"Oslo\"}"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}}`},
)

var responsesRefusalStream = namedFrames(
	namedEvent{"response.created", `{"type":"response.created","sequence_number":0,"response":{` + responseShell + `,"status":"in_progress","output":[]}}`},
	namedEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_fixture","type":"message","role":"assistant","status":"in_progress","content":[]}}`},
	namedEvent{"response.content_part.added", `{"type":"response.content_part.added","sequence_number":2,"item_id":"msg_fixture","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`},
	namedEvent{"response.refusal.delta", `{"type":"response.refusal.delta","sequence_number":3,"item_id":"msg_fixture","output_index":0,"content_index":0,"delta":"I cannot "}`},
	namedEvent{"response.refusal.delta", `{"type":"response.refusal.delta","sequence_number":4,"item_id":"msg_fixture","output_index":0,"content_index":0,"delta":"help"}`},
	namedEvent{"response.refusal.done", `{"type":"response.refusal.done","sequence_number":5,"item_id":"msg_fixture","output_index":0,"content_index":0,"refusal":"I cannot help"}`},
	namedEvent{"response.content_part.done", `{"type":"response.content_part.done","sequence_number":6,"item_id":"msg_fixture","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"I cannot help"}}`},
	namedEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I cannot help"}]}}`},
	namedEvent{"response.completed", `{"type":"response.completed","sequence_number":8,"response":{` + responseShell + `,"status":"completed","output":[{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I cannot help"}]}],"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}}`},
)

const anthropicStart = `{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture-upstream","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":3}}}`

var anthropicToolStream = namedFrames(
	namedEvent{"message_start", anthropicStart},
	namedEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`},
	namedEvent{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`},
	namedEvent{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2lnbmVk"}}`},
	namedEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	namedEvent{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
	namedEvent{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`},
	namedEvent{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`},
	namedEvent{"content_block_stop", `{"type":"content_block_stop","index":1}`},
	namedEvent{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_fixture","name":"lookup","input":{}}}`},
	namedEvent{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`},
	namedEvent{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"Oslo\"}"}}`},
	namedEvent{"content_block_stop", `{"type":"content_block_stop","index":2}`},
	namedEvent{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}`},
	namedEvent{"message_stop", `{"type":"message_stop"}`},
)

var anthropicRefusalStream = namedFrames(
	namedEvent{"message_start", anthropicStart},
	namedEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	namedEvent{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I cannot help"}}`},
	namedEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	namedEvent{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":4}}`},
	namedEvent{"message_stop", `{"type":"message_stop"}`},
)

const geminiChunk = `{"responseId":"gemini-fixture","modelVersion":"fixture-upstream","candidates":[{"index":0,"content":{"role":"model","parts":`

var geminiToolStream = dataFrames(
	geminiChunk+`[{"text":"plan","thought":true}]}}]}`,
	geminiChunk+`[{"text":"Hel"}]}}]}`,
	geminiChunk+`[{"text":"lo"}]}}]}`,
	geminiChunk+`[{"functionCall":{"name":"lookup","args":{"city":"Oslo"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":18,"cachedContentTokenCount":3}}`,
)

var geminiSafetyStream = dataFrames(
	geminiChunk+`[{"text":"Hel"}]}}]}`,
	geminiChunk+`[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":1,"totalTokenCount":12}}`,
)

var bedrockToolStream = bedrockFrames(
	namedEvent{"messageStart", `{"role":"assistant"}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"plan"}}}`},
	namedEvent{"contentBlockStop", `{"contentBlockIndex":0}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"Hel"}}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"text":"lo"}}`},
	namedEvent{"contentBlockStop", `{"contentBlockIndex":1}`},
	namedEvent{"contentBlockStart", `{"contentBlockIndex":2,"start":{"toolUse":{"toolUseId":"tooluse_fixture","name":"lookup"}}}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"{\"city\":"}}}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":2,"delta":{"toolUse":{"input":"\"Oslo\"}"}}}`},
	namedEvent{"contentBlockStop", `{"contentBlockIndex":2}`},
	namedEvent{"messageStop", `{"stopReason":"tool_use"}`},
	namedEvent{"metadata", `{"usage":{"inputTokens":11,"outputTokens":7,"totalTokens":18,"cacheReadInputTokens":3},"metrics":{"latencyMs":5}}`},
)

var bedrockGuardrailStream = bedrockFrames(
	namedEvent{"messageStart", `{"role":"assistant"}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"I cannot help"}}`},
	namedEvent{"contentBlockStop", `{"contentBlockIndex":0}`},
	namedEvent{"messageStop", `{"stopReason":"guardrail_intervened"}`},
	namedEvent{"metadata", `{"usage":{"inputTokens":11,"outputTokens":4,"totalTokens":15}}`},
)

var bedrockExceptionStream = append(bedrockFrames(
	namedEvent{"messageStart", `{"role":"assistant"}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`},
), bedrockMessage("exception", "throttlingException", `{"message":"slow down"}`)...)

var bedrockTruncatedStream = bedrockFrames(
	namedEvent{"messageStart", `{"role":"assistant"}`},
	namedEvent{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`},
)

const fimChunk = `{"id":"fim-fixture","object":"chat.completion.chunk","created":1,"model":"fixture-upstream","choices":[{"index":0,"delta":`

var mistralFIMStream = dataFrames(
	fimChunk+`{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
	fimChunk+`{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
	`[DONE]`,
)

var cohereToolStream = namedFrames(
	namedEvent{"message-start", `{"type":"message-start","id":"cohere-fixture","delta":{"message":{"role":"assistant","content":[],"tool_plan":"","tool_calls":[],"citations":[]}}}`},
	namedEvent{"content-start", `{"type":"content-start","index":0,"delta":{"message":{"content":{"type":"text","text":""}}}}`},
	namedEvent{"content-delta", `{"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"Hel"}}}}`},
	namedEvent{"content-delta", `{"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"lo"}}}}`},
	namedEvent{"content-end", `{"type":"content-end","index":0}`},
	namedEvent{"tool-call-start", `{"type":"tool-call-start","index":0,"delta":{"message":{"tool_calls":{"id":"call_fixture","type":"function","function":{"name":"lookup","arguments":""}}}}}`},
	namedEvent{"tool-call-delta", `{"type":"tool-call-delta","index":0,"delta":{"message":{"tool_calls":{"function":{"arguments":"{\"city\":\"Oslo\"}"}}}}}`},
	namedEvent{"tool-call-end", `{"type":"tool-call-end","index":0}`},
	namedEvent{"message-end", `{"type":"message-end","delta":{"finish_reason":"TOOL_CALL","usage":{"billed_units":{"input_tokens":11,"output_tokens":7},"tokens":{"input_tokens":13,"output_tokens":7}}}}`},
)

func streamSources() []streamSource {
	return []streamSource{
		{wire.Chat, []streamScenario{{"tool call", chatToolStream}, {"refusal", chatRefusalStream}, {"text", chatTextStream}}},
		{wire.Completions, []streamScenario{{"text", completionsStream}}},
		{wire.Responses, []streamScenario{{"tool call", responsesToolStream}, {"refusal", responsesRefusalStream}}},
		{wire.Anthropic, []streamScenario{{"tool call", anthropicToolStream}, {"refusal", anthropicRefusalStream}}},
		{wire.Gemini, []streamScenario{{"tool call", geminiToolStream}, {"safety stop", geminiSafetyStream}}},
		{wire.GeminiStream, []streamScenario{{"tool call", geminiToolStream}}},
		{wire.Bedrock, []streamScenario{{"tool call", bedrockToolStream}, {"guardrail stop", bedrockGuardrailStream}, {"exception", bedrockExceptionStream}, {"truncated", bedrockTruncatedStream}}},
		{wire.MistralFIM, []streamScenario{{"text", mistralFIMStream}}},
		{wire.CohereChat, []streamScenario{{"tool call", cohereToolStream}}},
	}
}

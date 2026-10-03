//go:build bench

package mockupstream

import (
	"net/http"
	"strconv"
)

// words are the completion's tokens: common English words, so each is about
// one token for a real tokenizer. Token i of every response is words[i%64]
// with a leading space, which makes a response a pure function of its length.
var words = [64]string{
	" the", " of", " and", " to", " in", " is", " you", " that",
	" it", " he", " was", " for", " on", " are", " as", " with",
	" his", " they", " at", " be", " this", " have", " from", " or",
	" one", " had", " by", " word", " but", " not", " what", " all",
	" were", " we", " when", " your", " can", " said", " there", " use",
	" an", " each", " which", " she", " do", " how", " their", " if",
	" will", " up", " other", " about", " out", " many", " then", " them",
	" these", " so", " some", " her", " would", " make", " like", " time",
}

// promptTokens is the mock's token accounting for a request: one token per
// four bytes of body, the gateway's own heuristic, so the count it reports
// follows the size of the body and the gateway's token accounting has
// something to be compared with.
func promptTokens(bodyBytes int64) int64 { return max(1, (bodyBytes+3)/4) }

// reply is what a response is rendered from.
type reply struct {
	model  []byte
	prompt int64
	output int
}

func (r *reply) total() int64 { return r.prompt + int64(r.output) }

// A dialect renders one provider's wire format. Every function appends to b,
// so a response is built in a single pooled buffer.
type dialect struct {
	name string
	// bodyModel reports that the request body, not the URL, names the model
	// and selects streaming.
	bodyModel bool
	unary     func(b []byte, r *reply) []byte
	// start is the stream's preamble, sent together with token 0; end is
	// everything after the last token, including its terminal event.
	start, end func(b []byte, r *reply) []byte
	token      func(b []byte, r *reply, i int) []byte
	// streamError is the dialect's in-band error event.
	streamError func(b []byte) []byte
	// errorBody renders an error response for status.
	errorBody func(b []byte, status int, message string) []byte
}

var (
	openAI    = &dialect{name: "openai", bodyModel: true, unary: openAIUnary, start: openAIStart, token: openAIToken, end: openAIEnd, streamError: openAIStreamError, errorBody: openAIError}
	anthropic = &dialect{name: "anthropic", bodyModel: true, unary: anthropicUnary, start: anthropicStart, token: anthropicToken, end: anthropicEnd, streamError: anthropicStreamError, errorBody: anthropicError}
	gemini    = &dialect{name: "gemini", unary: geminiUnary, start: geminiStart, token: geminiToken, end: geminiEnd, streamError: geminiStreamError, errorBody: geminiError}
	responses = &dialect{name: "responses", bodyModel: true, unary: responsesUnary, start: responsesStart, token: responsesToken, end: responsesEnd, streamError: responsesStreamError, errorBody: openAIError}
)

func appendInt(b []byte, v int64) []byte { return strconv.AppendInt(b, v, 10) }

func statusText(status int) string {
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "Error"
}

// failureMessage is the human text of every injected error.
func failureMessage(status int) string {
	return "injected " + strconv.Itoa(status) + " " + statusText(status)
}

// appendString appends s as a JSON string.
func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20:
			b = append(b, `\u00`...)
			b = append(b, "0123456789abcdef"[c>>4], "0123456789abcdef"[c&15])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}

// text appends the first n tokens of the completion.
func text(b []byte, n int) []byte {
	for i := 0; i < n; i++ {
		b = append(b, words[i&63]...)
	}
	return b
}

// OpenAI Chat Completions.

const openAIChunk = `data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1700000000,"model":"`

func openAIUnary(b []byte, r *reply) []byte {
	b = append(b, `{"id":"chatcmpl-mock","object":"chat.completion","created":1700000000,"model":"`...)
	b = append(b, r.model...)
	b = append(b, `","choices":[{"index":0,"message":{"role":"assistant","content":"`...)
	b = text(b, r.output)
	b = append(b, `"},"finish_reason":"stop"}],"usage":`...)
	b = openAIUsage(b, r)
	return append(b, '}')
}

func openAIUsage(b []byte, r *reply) []byte {
	b = append(b, `{"prompt_tokens":`...)
	b = appendInt(b, r.prompt)
	b = append(b, `,"completion_tokens":`...)
	b = appendInt(b, int64(r.output))
	b = append(b, `,"total_tokens":`...)
	b = appendInt(b, r.total())
	return append(b, '}')
}

func openAIStart(b []byte, r *reply) []byte {
	b = append(b, openAIChunk...)
	b = append(b, r.model...)
	return append(b, `","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n"...)
}

func openAIToken(b []byte, r *reply, i int) []byte {
	b = append(b, openAIChunk...)
	b = append(b, r.model...)
	b = append(b, `","choices":[{"index":0,"delta":{"content":"`...)
	b = append(b, words[i&63]...)
	return append(b, `"},"finish_reason":null}]}`+"\n\n"...)
}

// openAIEnd sends the finish chunk, then the usage chunk a client gets with
// stream_options.include_usage, then the terminator.
func openAIEnd(b []byte, r *reply) []byte {
	b = append(b, openAIChunk...)
	b = append(b, r.model...)
	b = append(b, `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"...)
	b = append(b, openAIChunk...)
	b = append(b, r.model...)
	b = append(b, `","choices":[],"usage":`...)
	b = openAIUsage(b, r)
	return append(b, "}\n\n"+`data: [DONE]`+"\n\n"...)
}

func openAIStreamError(b []byte) []byte {
	return append(b, `data: {"error":{"message":"injected mid-stream failure","type":"server_error","param":null,"code":"mock_stream_failure"}}`+"\n\n"...)
}

func openAIError(b []byte, status int, message string) []byte {
	kind, code := "server_error", "mock_error"
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		kind, code = "invalid_request_error", "mock_invalid_request"
	case http.StatusUnauthorized:
		kind, code = "invalid_request_error", "invalid_api_key"
	case http.StatusForbidden:
		kind, code = "invalid_request_error", "mock_permission_denied"
	case http.StatusTooManyRequests:
		kind, code = "rate_limit_error", "rate_limit_exceeded"
	}
	b = append(b, `{"error":{"message":`...)
	b = appendString(b, message)
	b = append(b, `,"type":"`...)
	b = append(b, kind...)
	b = append(b, `","param":null,"code":"`...)
	b = append(b, code...)
	return append(b, `"}}`...)
}

// OpenAI Responses. The scenarios drive Chat Completions, but a provider that
// speaks OpenAI is certified against both of its endpoints, so a provider in
// front of the mock needs this one to certify. It has no preamble: the first
// frame is the first token.

func responsesUnary(b []byte, r *reply) []byte {
	b = append(b, `{"id":"resp_mock","object":"response","created_at":1700000000,"status":"completed","error":null,"incomplete_details":null,"model":"`...)
	b = append(b, r.model...)
	b = append(b, `","output":[{"id":"msg_mock","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"`...)
	b = text(b, r.output)
	b = append(b, `","annotations":[]}]}],"usage":{"input_tokens":`...)
	b = appendInt(b, r.prompt)
	b = append(b, `,"output_tokens":`...)
	b = appendInt(b, int64(r.output))
	b = append(b, `,"total_tokens":`...)
	b = appendInt(b, r.total())
	return append(b, `}}`...)
}

func responsesStart(b []byte, _ *reply) []byte { return b }

func responsesToken(b []byte, _ *reply, i int) []byte {
	b = append(b, "event: response.output_text.delta\ndata: "+`{"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":"`...)
	b = append(b, words[i&63]...)
	return append(b, `"}`+"\n\n"...)
}

// responsesEnd is response.completed, which carries the whole response.
func responsesEnd(b []byte, r *reply) []byte {
	b = append(b, "event: response.completed\ndata: "+`{"type":"response.completed","response":`...)
	b = responsesUnary(b, r)
	return append(b, `}`+"\n\n"...)
}

func responsesStreamError(b []byte) []byte {
	return append(b, "event: error\ndata: "+`{"type":"error","code":"mock_stream_failure","message":"injected mid-stream failure","param":null}`+"\n\n"...)
}

// Anthropic Messages.

func anthropicUnary(b []byte, r *reply) []byte {
	b = append(b, `{"id":"msg_mock","type":"message","role":"assistant","model":"`...)
	b = append(b, r.model...)
	b = append(b, `","content":[{"type":"text","text":"`...)
	b = text(b, r.output)
	b = append(b, `"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":`...)
	b = appendInt(b, r.prompt)
	b = append(b, `,"output_tokens":`...)
	b = appendInt(b, int64(r.output))
	return append(b, `}}`...)
}

// anthropicStart is message_start, content_block_start and a ping.
func anthropicStart(b []byte, r *reply) []byte {
	b = append(b, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","model":"`...)
	b = append(b, r.model...)
	b = append(b, `","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":`...)
	b = appendInt(b, r.prompt)
	b = append(b, `,"output_tokens":1}}}`+"\n\n"...)
	b = append(b, "event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n"...)
	return append(b, "event: ping\ndata: "+`{"type":"ping"}`+"\n\n"...)
}

func anthropicToken(b []byte, _ *reply, i int) []byte {
	b = append(b, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`...)
	b = append(b, words[i&63]...)
	return append(b, `"}}`+"\n\n"...)
}

func anthropicEnd(b []byte, r *reply) []byte {
	b = append(b, "event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`+"\n\n"...)
	b = append(b, "event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":`...)
	b = appendInt(b, int64(r.output))
	b = append(b, `}}`+"\n\n"...)
	return append(b, "event: message_stop\ndata: "+`{"type":"message_stop"}`+"\n\n"...)
}

func anthropicStreamError(b []byte) []byte {
	return append(b, "event: error\ndata: "+`{"type":"error","error":{"type":"overloaded_error","message":"injected mid-stream failure"}}`+"\n\n"...)
}

func anthropicError(b []byte, status int, message string) []byte {
	kind := "api_error"
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		kind = "invalid_request_error"
	case http.StatusUnauthorized:
		kind = "authentication_error"
	case http.StatusForbidden:
		kind = "permission_error"
	case http.StatusNotFound:
		kind = "not_found_error"
	case http.StatusRequestEntityTooLarge:
		kind = "request_too_large"
	case http.StatusTooManyRequests:
		kind = "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		kind = "overloaded_error"
	}
	b = append(b, `{"type":"error","error":{"type":"`...)
	b = append(b, kind...)
	b = append(b, `","message":`...)
	b = appendString(b, message)
	return append(b, `}}`...)
}

// Gemini generateContent and streamGenerateContent. Streaming frames end in
// CRLF CRLF, as Google's do.

func geminiUnary(b []byte, r *reply) []byte {
	b = append(b, `{"candidates":[{"content":{"role":"model","parts":[{"text":"`...)
	b = text(b, r.output)
	b = append(b, `"}]},"finishReason":"STOP","index":0}],"usageMetadata":`...)
	b = geminiUsage(b, r)
	b = append(b, `,"modelVersion":"`...)
	b = append(b, r.model...)
	return append(b, `"}`...)
}

func geminiUsage(b []byte, r *reply) []byte {
	b = append(b, `{"promptTokenCount":`...)
	b = appendInt(b, r.prompt)
	b = append(b, `,"candidatesTokenCount":`...)
	b = appendInt(b, int64(r.output))
	b = append(b, `,"totalTokenCount":`...)
	b = appendInt(b, r.total())
	return append(b, '}')
}

// geminiStart is empty: Gemini has no preamble, so token 0 is the first frame.
func geminiStart(b []byte, _ *reply) []byte { return b }

func geminiToken(b []byte, r *reply, i int) []byte {
	b = append(b, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"`...)
	b = append(b, words[i&63]...)
	b = append(b, `"}]},"index":0}],"modelVersion":"`...)
	b = append(b, r.model...)
	return append(b, `"}`+"\r\n\r\n"...)
}

// geminiEnd is the closing chunk: an empty part, the finish reason and usage.
func geminiEnd(b []byte, r *reply) []byte {
	b = append(b, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP","index":0}],"usageMetadata":`...)
	b = geminiUsage(b, r)
	b = append(b, `,"modelVersion":"`...)
	b = append(b, r.model...)
	return append(b, `"}`+"\r\n\r\n"...)
}

func geminiStreamError(b []byte) []byte {
	return append(b, `data: {"error":{"code":503,"message":"injected mid-stream failure","status":"UNAVAILABLE"}}`+"\r\n\r\n"...)
}

func geminiError(b []byte, status int, message string) []byte {
	state := "UNKNOWN"
	switch status {
	case http.StatusBadRequest:
		state = "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		state = "UNAUTHENTICATED"
	case http.StatusForbidden:
		state = "PERMISSION_DENIED"
	case http.StatusNotFound:
		state = "NOT_FOUND"
	case http.StatusTooManyRequests:
		state = "RESOURCE_EXHAUSTED"
	case http.StatusInternalServerError:
		state = "INTERNAL"
	case http.StatusServiceUnavailable:
		state = "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		state = "DEADLINE_EXCEEDED"
	}
	b = append(b, `{"error":{"code":`...)
	b = appendInt(b, int64(status))
	b = append(b, `,"message":`...)
	b = appendString(b, message)
	b = append(b, `,"status":"`...)
	b = append(b, state...)
	return append(b, `"}}`...)
}

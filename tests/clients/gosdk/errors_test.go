//go:build integration

package gosdk

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"google.golang.org/genai"
)

// An application catches its SDK's typed error, not "some rejection", so a
// gateway whose failures do not land in that type with the status and fields
// the vendor documents is not compatible however well its successes are
// shaped. Each case below is a condition every SDK sees, run through each SDK
// in its own call shape, unary and streaming.

// failure is what an SDK reports of an API error, in one shape.
type failure struct {
	status int
	// kind is the vendor's own classification: the type of an OpenAI or
	// Anthropic error, the status string of a Google one.
	kind string
	// code is the machine-readable code the gateway sent, wherever the SDK
	// exposes it: the code of an OpenAI error, the code member of an Anthropic
	// error, the reason of a Google error's ErrorInfo.
	code string
	// param is the request parameter the gateway blamed.
	param      string
	message    string
	retryAfter string
	// requestID is the request ID of the error: the X-Request-Id the gateway
	// sent, which the Anthropic SDK reports as the RequestID of its error and
	// the OpenAI SDK leaves to its response.
	requestID string
}

// sdk is one official SDK, driven the same way for every case.
type sdk struct {
	name string
	// call makes a generation request through the SDK and returns the error it
	// reported. malformed asks for a request the gateway itself refuses.
	call   func(t *testing.T, h harness, key, model, prompt string, malformed bool) error
	stream func(t *testing.T, h harness, key, model, prompt string, malformed bool) error
	// observe reads the typed error out of err, failing the test when err is
	// not the SDK's own API error type.
	observe func(t *testing.T, err error) failure
	// kinds maps an HTTP status to the vendor's classification of it.
	kinds map[int]string
	// route is the transformed route this vendor's client speaks to, and the
	// code and parameter the gateway blames for a malformed request.
	route          string
	malformedCode  string
	malformedParam string
}

var sdks = []sdk{
	{
		name: "openai-go", route: "openai", malformedCode: "missing_required_parameter", malformedParam: "messages",
		call: func(t *testing.T, h harness, key, model, prompt string, malformed bool) error {
			_, err := h.openAI(openaioption.WithAPIKey(key)).Chat.Completions.New(timeout(t), openAIChat(model, prompt, malformed))
			return err
		},
		stream: func(t *testing.T, h harness, key, model, prompt string, malformed bool) error {
			stream := h.openAI(openaioption.WithAPIKey(key)).Chat.Completions.NewStreaming(timeout(t), openAIChat(model, prompt, malformed))
			defer stream.Close()
			for stream.Next() {
				t.Fatalf("the stream delivered a chunk before its error: %+v", stream.Current())
			}
			return stream.Err()
		},
		observe: func(t *testing.T, err error) failure {
			e, ok := errors.AsType[*openai.Error](err)
			if !ok {
				t.Fatalf("expected an *openai.Error, got %T: %v", err, err)
			}
			return failure{
				status: e.StatusCode, kind: e.Type, code: e.Code, param: e.Param, message: e.Message,
				retryAfter: e.Response.Header.Get("Retry-After"), requestID: e.Response.Header.Get("X-Request-Id"),
			}
		},
		kinds: map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "invalid_request_error", 429: "rate_limit_error", 502: "server_error"},
	},
	{
		name: "anthropic-sdk-go", route: "anthropic", malformedCode: "unsupported_parameter", malformedParam: "max_tokens",
		call: func(t *testing.T, h harness, key, model, prompt string, malformed bool) error {
			_, err := h.anthropic(anthropicoption.WithAPIKey(key)).Messages.New(timeout(t), anthropicMessage(model, prompt, malformed))
			return err
		},
		stream: func(t *testing.T, h harness, key, model, prompt string, malformed bool) error {
			stream := h.anthropic(anthropicoption.WithAPIKey(key)).Messages.NewStreaming(timeout(t), anthropicMessage(model, prompt, malformed))
			defer stream.Close()
			for stream.Next() {
				t.Fatalf("the stream delivered an event before its error: %+v", stream.Current())
			}
			return stream.Err()
		},
		observe: func(t *testing.T, err error) failure {
			e, ok := errors.AsType[*anthropic.Error](err)
			if !ok {
				t.Fatalf("expected an *anthropic.Error, got %T: %v", err, err)
			}
			// The SDK exposes the type of the error; the rest of the envelope
			// is in the raw body.
			var envelope struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
					Code    string `json:"code"`
					Param   string `json:"param"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(e.RawJSON()), &envelope); err != nil || envelope.Type != "error" {
				t.Fatalf("the error body %q is not the Anthropic envelope: %v", e.RawJSON(), err)
			}
			if string(e.Type()) != envelope.Error.Type {
				t.Fatalf("the SDK reports type %q for an envelope of type %q", e.Type(), envelope.Error.Type)
			}
			return failure{
				status: e.StatusCode, kind: string(e.Type()), code: envelope.Error.Code, param: envelope.Error.Param, message: envelope.Error.Message,
				retryAfter: e.Response.Header.Get("Retry-After"), requestID: reportedRequestID(t, e.RequestID, e.Response),
			}
		},
		kinds: map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 429: "rate_limit_error", 502: "api_error"},
	},
	{
		name: "genai", route: "gemini", malformedCode: "unsupported_parameter", malformedParam: "contents",
		call: func(t *testing.T, h harness, key, model, prompt string, malformed bool) error {
			_, err := h.genai(t, key).Models.GenerateContent(timeout(t), model, geminiContents(prompt, malformed), nil)
			return err
		},
		stream: func(t *testing.T, h harness, key, model, prompt string, malformed bool) error {
			for response, err := range h.genai(t, key).Models.GenerateContentStream(timeout(t), model, geminiContents(prompt, malformed), nil) {
				if err == nil {
					t.Fatalf("the stream delivered a chunk before its error: %+v", response)
				}
				return err
			}
			t.Fatal("the stream ended without an error")
			return nil
		},
		observe: func(t *testing.T, err error) failure {
			e, ok := errors.AsType[genai.APIError](err)
			if !ok {
				t.Fatalf("expected a genai.APIError, got %T: %v", err, err)
			}
			out := failure{status: e.Code, kind: e.Status, message: e.Message}
			// The gateway puts its code in the ErrorInfo detail, as Google does.
			for _, detail := range e.Details {
				if detail["@type"] == "type.googleapis.com/google.rpc.ErrorInfo" {
					out.code, _ = detail["reason"].(string)
					if metadata, ok := detail["metadata"].(map[string]any); ok {
						out.param, _ = metadata["field"].(string)
					}
				}
			}
			return out
		},
		kinds: map[int]string{400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED", 404: "NOT_FOUND", 429: "RESOURCE_EXHAUSTED", 502: "INTERNAL"},
	},
}

// reportedRequestID checks that the request ID an SDK reports for an error is
// the one the gateway sent as X-Request-Id, the ID a user quotes to support, and
// returns it. The Anthropic SDK reads it from the request-id header, which the
// gateway sends on the Anthropic surface.
func reportedRequestID(t *testing.T, reported string, response *http.Response) string {
	t.Helper()
	if sent := response.Header.Get("X-Request-Id"); reported != sent {
		t.Fatalf("the SDK reports the request ID %q, the gateway sent %q", reported, sent)
	}
	return reported
}

func openAIChat(model, prompt string, malformed bool) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{Model: model, Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)}}
	if malformed {
		params.Messages = nil
	}
	return params
}

func anthropicMessage(model, prompt string, malformed bool) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model: anthropic.Model(model), MaxTokens: 64,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	}
	if malformed {
		params.MaxTokens = 0
	}
	return params
}

func geminiContents(prompt string, malformed bool) []*genai.Content {
	if malformed {
		// An empty list reaches the gateway as `"contents":null`; a nil one
		// sends no body at all.
		return []*genai.Content{}
	}
	return genai.Text(prompt)
}

// errorCase is a condition with the answer every SDK should see.
type errorCase struct {
	name string
	// key is the credential the request carries.
	key func(h harness) string
	// model is the route addressed instead of the SDK's own.
	model string
	// prompt scripts the upstream.
	prompt    string
	malformed bool
	status    int
	// code is the code of the gateway's error; empty takes the SDK's own, for a
	// request each vendor's codec blames differently.
	code string
	// forwarded is the status the upstream answered with, and zero when the
	// gateway refused the request itself and never reached it.
	forwarded int
}

var errorCases = []errorCase{
	{name: "an unknown key is 401", key: func(h harness) string { return invalidKey }, status: 401, code: "invalid_api_key"},
	{name: "a key not allowed on the route is 403", key: func(h harness) string { return h.restrictedKey }, status: 403, code: "route_forbidden"},
	{name: "an unknown route is 404", model: "no-such-route", status: 404, code: "route_not_found"},
	{name: "a malformed request is 400", malformed: true, status: 400},
	{name: "a request the upstream rejects is 400", prompt: fail(400), status: 400, code: "upstream_rejected", forwarded: 400},
	{name: "an upstream rate limit is 429", prompt: fail(429), status: 429, code: "upstream_rate_limit", forwarded: 429},
	{name: "an upstream failure is 502", prompt: fail(500), status: 502, code: "upstream_unavailable", forwarded: 500},
}

func TestSDKsReportTheGatewaysErrorsAsTheirTypedErrors(t *testing.T) {
	for _, s := range sdks {
		for _, c := range errorCases {
			for _, streaming := range []bool{false, true} {
				name := s.name + "/" + c.name
				if streaming {
					name += "/streaming"
				}
				t.Run(name, func(t *testing.T) {
					h := connect(t)
					key, model, prompt := h.apiKey, h.models[s.route], "Say hello."
					if c.key != nil {
						key = c.key(h)
					}
					if c.model != "" {
						model = c.model
					}
					if c.prompt != "" {
						prompt = c.prompt
					}
					call := s.call
					if streaming {
						call = s.stream
					}
					if c.status == 429 {
						// The slot is parked for the upstream's Retry-After; leave it
						// serving for whatever uses the route next.
						t.Cleanup(func() {
							awaitServing(t, func() error { return s.call(t, h, h.apiKey, h.models[s.route], "Say hello.", false) })
						})
					}

					got := s.observe(t, call(t, h, key, model, prompt, c.malformed))
					if got.status != c.status || got.kind != s.kinds[c.status] {
						t.Fatalf("status %d (%s), want %d (%s): %+v", got.status, got.kind, c.status, s.kinds[c.status], got)
					}
					wantCode, wantParam := c.code, ""
					if c.malformed {
						wantCode, wantParam = s.malformedCode, s.malformedParam
					}
					if got.code != wantCode || got.param != wantParam || got.message == "" {
						t.Fatalf("code %q, param %q, message %q; want code %q, param %q: %+v", got.code, got.param, got.message, wantCode, wantParam, got)
					}
					if s.name != "genai" && got.requestID == "" {
						t.Fatalf("the error carries no request ID: %+v", got)
					}
					if c.status == 429 && got.retryAfter != "1" && s.name != "genai" {
						t.Fatalf("the rate limit does not say when to retry: %+v", got)
					}

					if c.forwarded == 0 {
						h.untouched(t)
					} else if r := h.refused(t, c.forwarded); r.Stream != streaming {
						t.Fatalf("upstream saw %+v", r)
					}
				})
			}
		}
	}
}

// Every other endpoint of an SDK reports the same typed errors, since the
// error contract belongs to the surface and not to one operation.

func TestOpenAIEndpointsReportTypedErrors(t *testing.T) {
	h := connect(t)
	client := h.openAI()
	missing := func(name string, err error) {
		t.Helper()
		e, ok := errors.AsType[*openai.Error](err)
		if !ok || e.StatusCode != 404 || e.Code != "route_not_found" || e.Type != "invalid_request_error" {
			t.Fatalf("%s: %T %v", name, err, err)
		}
	}
	_, err := client.Responses.New(timeout(t), responses.ResponseNewParams{
		Model: "no-such-route", Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hi")},
	})
	missing("responses", err)
	_, err = client.Embeddings.New(timeout(t), openai.EmbeddingNewParams{
		Model: "no-such-route", Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String("hi")},
	})
	missing("embeddings", err)
	_, err = client.Models.Get(timeout(t), "no-such-route")
	missing("model retrieval", err)

	// An invalid key is refused before any endpoint reads its body.
	_, err = h.openAI(openaioption.WithAPIKey(invalidKey)).Embeddings.New(timeout(t), openai.EmbeddingNewParams{
		Model: h.models["openai"], Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String("hi")},
	})
	if e, ok := errors.AsType[*openai.Error](err); !ok || e.StatusCode != 401 || e.Code != "invalid_api_key" {
		t.Fatalf("embeddings with an invalid key: %T %v", err, err)
	}
	h.untouched(t)
}

func TestAnthropicEndpointsReportTypedErrors(t *testing.T) {
	h := connect(t)
	_, err := h.anthropic().Messages.CountTokens(timeout(t), anthropic.MessageCountTokensParams{
		Model: "no-such-route", Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if e, ok := errors.AsType[*anthropic.Error](err); !ok || e.StatusCode != 404 || e.Type() != "not_found_error" || e.RequestID == "" {
		t.Fatalf("count_tokens: %T %v", err, err)
	}
	_, err = h.anthropic(anthropicoption.WithAPIKey(invalidKey)).Messages.CountTokens(timeout(t), anthropic.MessageCountTokensParams{
		Model: anthropic.Model(h.models["anthropic"]), Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if e, ok := errors.AsType[*anthropic.Error](err); !ok || e.StatusCode != 401 || e.Type() != "authentication_error" || e.RequestID == "" {
		t.Fatalf("count_tokens with an invalid key: %T %v", err, err)
	}
	h.untouched(t)
}

func TestGeminiEndpointsReportTypedErrors(t *testing.T) {
	h := connect(t)
	client := h.genai(t, h.apiKey)
	_, err := client.Models.CountTokens(timeout(t), "no-such-route", genai.Text("hi"), nil)
	if e, ok := errors.AsType[genai.APIError](err); !ok || e.Code != 404 || e.Status != "NOT_FOUND" {
		t.Fatalf("countTokens: %T %v", err, err)
	}
	_, err = client.Models.EmbedContent(timeout(t), "no-such-route", genai.Text("hi"), nil)
	if e, ok := errors.AsType[genai.APIError](err); !ok || e.Code != 404 || e.Status != "NOT_FOUND" {
		t.Fatalf("embedContent: %T %v", err, err)
	}
	_, err = h.genai(t, invalidKey).Models.CountTokens(timeout(t), h.models["gemini"], genai.Text("hi"), nil)
	if e, ok := errors.AsType[genai.APIError](err); !ok || e.Code != 401 || e.Status != "UNAUTHENTICATED" {
		t.Fatalf("countTokens with an invalid key: %T %v", err, err)
	}
	h.untouched(t)
}

// An SDK retries a 429 after the Retry-After the gateway relays from the
// upstream, so the header is a contract and not decoration. The scripted
// upstream rate-limits every attempt, so the SDK gives up after its retries.

func TestOpenAIRetriesARateLimitAfterTheRetryAfterItIsGiven(t *testing.T) {
	h := connect(t)
	t.Cleanup(func() {
		awaitServing(t, func() error {
			_, err := h.openAI().Chat.Completions.New(timeout(t), openAIChat(h.models["openai"], "Say hello.", false))
			return err
		})
	})
	started := time.Now()
	_, err := h.openAI(openaioption.WithMaxRetries(1)).Chat.Completions.New(timeout(t), openAIChat(h.models["openai"], fail(429), false))
	elapsed := time.Since(started)
	if e, ok := errors.AsType[*openai.Error](err); !ok || e.StatusCode != 429 {
		t.Fatalf("%T %v", err, err)
	}
	if elapsed < time.Second {
		t.Fatalf("the SDK retried after %v, before the Retry-After of one second", elapsed)
	}
	requests := h.recorded(t, nil)
	if len(requests) != 2 {
		t.Fatalf("the upstream saw %d requests, want the call and one retry", len(requests))
	}
	h.clean(t, requests, 429)
}

func TestAnthropicRetriesARateLimitAfterTheRetryAfterItIsGiven(t *testing.T) {
	h := connect(t)
	t.Cleanup(func() {
		awaitServing(t, func() error {
			_, err := h.anthropic().Messages.New(timeout(t), anthropicMessage(h.models["anthropic"], "Say hello.", false))
			return err
		})
	})
	started := time.Now()
	_, err := h.anthropic(anthropicoption.WithMaxRetries(1)).Messages.New(timeout(t), anthropicMessage(h.models["anthropic"], fail(429), false))
	elapsed := time.Since(started)
	if e, ok := errors.AsType[*anthropic.Error](err); !ok || e.StatusCode != 429 || e.Type() != "rate_limit_error" {
		t.Fatalf("%T %v", err, err)
	}
	if elapsed < time.Second {
		t.Fatalf("the SDK retried after %v, before the Retry-After of one second", elapsed)
	}
	requests := h.recorded(t, nil)
	if len(requests) != 2 {
		t.Fatalf("the upstream saw %d requests, want the call and one retry", len(requests))
	}
	h.clean(t, requests, 429)
}

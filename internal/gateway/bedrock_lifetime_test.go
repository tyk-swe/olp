package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"

	"github.com/tyk-swe/olp/internal/protocols"
)

func TestBedrockRelayKeepsPartialUsageUncertainUntilNativeTerminal(t *testing.T) {
	for _, mode := range []string{"partial EOF", "malformed frame", "valid terminal", "terminal delivery failure", "terminal flush failure"} {
		t.Run(mode, func(t *testing.T) {
			h, x, p := pinnedExchangeFixture(t)
			x.family, x.mode = wire.BedrockInvoke, "streaming"
			x.dispatched = true
			e := h.gateway.beginPinned(t.Context(), x, p, http.MethodPost)
			e.state.accepted = true
			e.state.dispatched.Store(true)
			var body bytes.Buffer
			encoder := eventstream.NewEncoder()
			start := bedrockChunk(t, `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}`)
			if mode != "terminal delivery failure" && mode != "terminal flush failure" {
				if err := encoder.Encode(&body, *start); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "malformed frame" {
				body.Write([]byte{0, 0, 0, 24})
			}
			complete := mode == "valid terminal" || mode == "terminal delivery failure" || mode == "terminal flush failure"
			if complete {
				stop := bedrockChunk(t, `{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":11,"outputTokenCount":7}}`)
				if err := encoder.Encode(&body, *stop); err != nil {
					t.Fatal(err)
				}
			}
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&body)}
			var w http.ResponseWriter = httptest.NewRecorder()
			if mode == "terminal delivery failure" {
				w = &failedMediaWriter{header: make(http.Header)}
			} else if mode == "terminal flush failure" {
				w = &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: io.ErrClosedPipe, beforeDelivery: func() {}}
			}
			h.gateway.relayBedrockStream(t.Context(), w, x, p, resp)
			fact := x.facts[0]
			if mode == "terminal flush failure" && h.sink.last(t).FirstByte != nil {
				t.Fatal("failed buffered flush claimed a delivered first byte")
			}
			if fact.Usage == nil || !fact.UsageObserved || fact.UsageComplete != complete || fact.BillingUncertain == complete {
				t.Fatalf("wrong usage disposition: %+v", fact)
			}
			if e.state.settled != complete {
				t.Fatalf("header/partial observation became completion: %+v", fact)
			}
			if mode == "valid terminal" {
				if x.failure != nil || fact.Class != classSuccess {
					t.Fatalf("terminal failed: %+v, %+v", fact, x.failure)
				}
			} else if x.failure == nil || fact.Class == classSuccess {
				t.Fatalf("failed exchange became successful: %+v", fact)
			}
		})
	}
}

func TestBedrockNativeFailuresScrubAppliedCredentials(t *testing.T) {
	for _, mode := range []string{"exception", "model error"} {
		t.Run(mode, func(t *testing.T) {
			h, x, p := pinnedExchangeFixture(t)
			x.family = wire.BedrockInvoke
			x.sensitive.Add("applied-provider-secret")
			e := h.gateway.beginPinned(t.Context(), x, p, http.MethodPost)
			e.state.accepted = true
			e.state.dispatched.Store(true)
			message := bedrockChunk(t, `{"type":"error","error":{"message":"echo applied-provider-secret"}}`)
			if mode == "exception" {
				message = bedrockEvent(t, "error", map[string]string{"message": "echo applied-provider-secret"})
				message.Headers.Set(":message-type", eventstream.StringValue("exception"))
				message.Headers.Set(":exception-type", eventstream.StringValue("echo applied-provider-secret"))
			}
			var body bytes.Buffer
			if err := eventstream.NewEncoder().Encode(&body, *message); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.gateway.relayBedrockStream(t.Context(), w, x, p, &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&body)})
			forwarded, err := protocols.ReadBedrockEvent(w.Body, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			observation, err := protocols.ObserveBedrockEvent(&forwarded, 1<<20)
			if err != nil || !observation.Failed {
				t.Fatalf("native failure lost grammar: %+v %v", observation, err)
			}
			if bytes.Contains(forwarded.Payload, []byte("applied-provider-secret")) || bytes.Contains(observation.Native.Bytes(), []byte("applied-provider-secret")) {
				t.Fatal("credential leaked in native error")
			}
			for _, header := range forwarded.Headers {
				if x.sensitive.Contains(header.Value.String()) {
					t.Fatal("credential leaked in native error header")
				}
			}
			if x.facts[0].Class != classUpstreamServer || e.state.settled {
				t.Fatalf("native failure became completion: %+v", x.facts[0])
			}
		})
	}
}

func TestBedrockRelayRequiresExplicitUsageCounts(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		invoke                 bool
		start, delta, metadata string
		observed, complete     bool
		input, output          int64
	}{
		{name: "absent invoke counts", invoke: true, start: `{}`, delta: `{}`},
		{name: "explicit invoke zeros", invoke: true, start: `{"input_tokens":0,"output_tokens":0}`, delta: `{"output_tokens":0}`, observed: true, complete: true},
		{name: "partial invoke input", invoke: true, start: `{"input_tokens":3}`, delta: `{}`, observed: true, input: 3},
		{name: "delta zero is final", invoke: true, start: `{"input_tokens":3,"output_tokens":1}`, delta: `{"output_tokens":0}`, observed: true, complete: true, input: 3},
		{name: "absent Converse counts", metadata: `{}`},
		{name: "partial Converse input", metadata: `{"inputTokens":3}`, observed: true, input: 3},
		{name: "explicit Converse zeros", metadata: `{"inputTokens":0,"outputTokens":0}`, observed: true, complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, x, p := pinnedExchangeFixture(t)
			x.family, x.mode = wire.BedrockInvoke, "streaming"
			e := h.gateway.beginPinned(t.Context(), x, p, http.MethodPost)
			e.state.accepted = true
			e.state.dispatched.Store(true)
			var body bytes.Buffer
			write := func(message *eventstream.Message) {
				t.Helper()
				if err := eventstream.NewEncoder().Encode(&body, *message); err != nil {
					t.Fatal(err)
				}
			}
			if tc.invoke {
				write(bedrockChunk(t, `{"type":"message_start","message":{"usage":`+tc.start+`}}`))
				write(bedrockChunk(t, `{"type":"message_delta","usage":`+tc.delta+`}`))
				write(bedrockChunk(t, `{"type":"message_stop"}`))
			} else {
				write(bedrockEvent(t, "messageStop", map[string]string{"stopReason": "end_turn"}))
				write(bedrockMetadata(`{"usage":` + tc.metadata + `}`))
			}
			h.gateway.relayBedrockStream(t.Context(), httptest.NewRecorder(), x, p, &http.Response{Header: make(http.Header), Body: io.NopCloser(&body)})
			fact := x.facts[0]
			if fact.UsageObserved != tc.observed || fact.UsageComplete != tc.complete || fact.BillingUncertain == tc.complete {
				t.Fatalf("presence was lost: %+v", fact)
			}
			if tc.observed && (fact.Usage == nil || fact.Usage.InputTokens != tc.input || fact.Usage.OutputTokens != tc.output) {
				t.Fatalf("counts were lost: %+v", fact.Usage)
			}
			if !e.state.settled || x.failure != nil {
				t.Fatalf("valid native terminal lost: settled=%t failure=%v", e.state.settled, x.failure)
			}
		})
	}
}

func TestBedrockRelayRetainsKnownCountsAfterMalformedUsage(t *testing.T) {
	for _, count := range []string{"-1", "1.5", "9223372036854775808"} {
		t.Run(count, func(t *testing.T) {
			h, x, p := pinnedExchangeFixture(t)
			x.family, x.mode = wire.BedrockInvoke, "streaming"
			e := h.gateway.beginPinned(t.Context(), x, p, http.MethodPost)
			e.state.accepted = true
			e.state.dispatched.Store(true)
			var body bytes.Buffer
			for _, message := range []*eventstream.Message{
				bedrockChunk(t, `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}`),
				bedrockChunk(t, `{"type":"message_delta","usage":{"output_tokens":`+count+`}}`),
				bedrockChunk(t, `{"type":"message_stop"}`),
			} {
				if err := eventstream.NewEncoder().Encode(&body, *message); err != nil {
					t.Fatal(err)
				}
			}
			h.gateway.relayBedrockStream(t.Context(), httptest.NewRecorder(), x, p, &http.Response{Header: make(http.Header), Body: io.NopCloser(&body)})
			fact := x.facts[0]
			if fact.Usage == nil || fact.Usage.InputTokens != 11 || fact.Usage.OutputTokens != 1 || !fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain || fact.Class != classProtocol || x.failure == nil || e.state.settled {
				t.Fatalf("malformed count erased or completed known usage: %+v", fact)
			}
		})
	}
}

func TestBedrockUnaryFlushFailureRetainsUpstreamUsage(t *testing.T) {
	h := newHarness(t, Config{})
	raw, err := json.Marshal(h.rt.release.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot routing.Snapshot
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	route := snapshot.Routes[routeSlug]
	route.Targets = route.Targets[:1]
	provider := snapshot.Providers[route.Targets[0].ProviderID]
	const model = "anthropic.claude-3-5-sonnet-v2:0"
	provider.Kind, provider.ProfileID, provider.ProfileRevision = "bedrock", "bedrock-invoke", connectors.ProfileRevision
	provider.AuthMode, provider.CloudRegion, provider.Endpoint = "static", "us-east-1", h.upstream.URL+"/a"
	provider.Capabilities = []routing.Capability{{Model: model, Operation: "bedrock_invoke", Surface: "bedrock", Mode: "unary"}}
	route.Operations, route.Targets[0].ProviderModel = []string{"bedrock_invoke"}, model
	snapshot.Providers[provider.ID], snapshot.Routes[route.Slug] = provider, route
	h.rt.release, err = runtime.NewRelease(uuid.NewString(), 8, &snapshot, map[string][]byte{h.credA: []byte(`{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`)})
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", status(http.StatusOK, `{"id":"m","type":"message","role":"assistant","model":"`+model+`","content":[{"type":"text","text":"ready"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	r := httptest.NewRequest(http.MethodPost, "/bedrock/model/"+routeSlug+"/invoke", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`))
	r.SetPathValue("model", routeSlug)
	r.Header.Set("X-OLP-API-Key", fullKey)
	r.Header.Set("Content-Type", "application/json")
	w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: io.ErrClosedPipe, beforeDelivery: func() {}}
	h.gateway.bedrockServe(w, r, wire.BedrockInvoke, "bedrock_invoke", "unary", "invoke")
	env := h.sink.last(t)
	if env.Outcome != "cancelled" || !env.Committed || env.Status != 0 || len(env.Attempts) != 1 {
		t.Fatalf("buffered write claimed success: %+v", env)
	}
	fact := env.Attempts[0]
	if env.FirstByte != nil {
		t.Fatal("failed buffered flush claimed a delivered first byte")
	}
	if fact.Usage == nil || fact.Usage.InputTokens != 3 || fact.Usage.OutputTokens != 2 || !fact.UsageComplete || fact.BillingUncertain {
		t.Fatalf("flush failure lost terminal accounting: %+v", fact)
	}
}

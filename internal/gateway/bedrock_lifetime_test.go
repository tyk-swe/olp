package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/protocols"
)

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

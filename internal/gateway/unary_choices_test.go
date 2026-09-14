package gateway

import (
	"net/http"
	"testing"
)

func TestMalformedUnaryChoicesRecordTerminalProtocolFailure(t *testing.T) {
	for _, choices := range []string{
		`[{}]`,
		`[{"index":0,"message":null,"finish_reason":"stop"}]`,
		`[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"},{}]`,
	} {
		t.Run(choices, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", status(http.StatusOK, `{"choices":`+choices+`}`))
			resp, body := h.chat(fullKey, nil)
			if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "provider_protocol_error" {
				t.Fatalf("malformed completion reached the client: %d %v", resp.StatusCode, body)
			}
			env := h.sink.last(t)
			if env.Outcome != "failure" || env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || h.mock.count("b") != 0 {
				t.Fatalf("malformed completion was not a terminal failure: %+v", env)
			}
		})
	}
}

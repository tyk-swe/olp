//go:build bench

package mockupstream

import (
	"bytes"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/protocols"
)

// The gateway is the mock's first client, so the mock is held to the
// gateway's own decoders: whatever OLP's codecs accept from a real upstream
// they must accept from the mock, with the usage and text the mock claims,
// natively and when translated to every other surface.

var wires = []struct {
	name   string
	family wire.Format
	unary  request
	stream request
}{
	{"openai", wire.Chat, request{path: "/v1/chat/completions", body: openAIBody("route-model", false, "hello")}, request{path: "/v1/chat/completions", body: openAIBody("route-model", true, "hello")}},
	{"responses", wire.Responses, request{path: "/v1/responses", body: responsesBody("route-model", false, "hello")}, request{path: "/v1/responses", body: responsesBody("route-model", true, "hello")}},
	{"anthropic", wire.Anthropic, request{path: "/v1/messages", body: anthropicBody("route-model", false, "hello")}, request{path: "/v1/messages", body: anthropicBody("route-model", true, "hello")}},
	{"gemini", wire.Gemini, request{path: "/v1beta/models/route-model:generateContent", body: geminiBody}, request{path: "/v1beta/models/route-model:streamGenerateContent?alt=sse", body: geminiBody}},
}

// TestTruncatedStreamsAreProtocolErrors proves the failure modes are ones the
// gateway tells apart from success: a stream cut short is an error, not a
// short answer, and an in-band error event is an upstream error.
func TestTruncatedStreamsAreProtocolErrors(t *testing.T) {
	s := defaultMock(t)
	for _, w := range wires {
		t.Run(w.name+"/error frame", func(t *testing.T) {
			req := w.stream
			req.headers = map[string]string{"x-mock-fail-after-tokens": "3", "x-mock-fail-mode": "error"}
			_, body := s.post(t, req)
			_, err := protocols.Stream(w.family, w.family, bytes.NewReader(body), 1<<20, "route", true, func([]byte) error { return nil })
			if err == nil {
				t.Fatalf("an in-band failure decoded as success:\n%s", body)
			}
		})
	}
}

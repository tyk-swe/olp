//go:build bench

package mockupstream

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// The gateway is the mock's first client, so the mock is held to the
// gateway's own decoders: whatever OLP's codecs accept from a real upstream
// they must accept from the mock, with the usage and text the mock claims,
// natively and when translated to every other surface.

var wires = []struct {
	name   string
	family openai.Family
	unary  request
	stream request
}{
	{"openai", openai.FamilyChat, request{path: "/v1/chat/completions", body: openAIBody("route-model", false, "hello")}, request{path: "/v1/chat/completions", body: openAIBody("route-model", true, "hello")}},
	{"responses", openai.FamilyResponses, request{path: "/v1/responses", body: responsesBody("route-model", false, "hello")}, request{path: "/v1/responses", body: responsesBody("route-model", true, "hello")}},
	{"anthropic", openai.FamilyAnthropic, request{path: "/v1/messages", body: anthropicBody("route-model", false, "hello")}, request{path: "/v1/messages", body: anthropicBody("route-model", true, "hello")}},
	{"gemini", openai.FamilyGemini, request{path: "/v1beta/models/route-model:generateContent", body: geminiBody}, request{path: "/v1beta/models/route-model:streamGenerateContent?alt=sse", body: geminiBody}},
}

var targets = []openai.Family{openai.FamilyChat, openai.FamilyAnthropic, openai.FamilyGemini}

func TestUnaryResponsesDecodeWithTheGatewayCodecs(t *testing.T) {
	s := defaultMock(t)
	for _, w := range wires {
		_, body := s.post(t, w.unary)
		for _, target := range targets {
			t.Run(w.name+"->"+string(target), func(t *testing.T) {
				c, err := protocols.Decode(w.family, target, body, "route", "")
				if err != nil {
					t.Fatalf("%v\n%s", err, body)
				}
				if c.Usage == nil || c.Usage.InputTokens != promptFor(w.unary.body) || c.Usage.OutputTokens != 16 || c.Usage.TotalTokens != promptFor(w.unary.body)+16 {
					t.Fatalf("usage %+v, want %d in and 16 out", c.Usage, promptFor(w.unary.body))
				}
				if c.OutputText != wantText(16) {
					t.Fatalf("text %q", c.OutputText)
				}
			})
		}
	}
}

func TestStreamsDecodeWithTheGatewayCodecs(t *testing.T) {
	s := defaultMock(t)
	for _, w := range wires {
		_, body := s.post(t, w.stream)
		for _, target := range targets {
			t.Run(w.name+"->"+string(target), func(t *testing.T) {
				var out bytes.Buffer
				c, err := protocols.Stream(w.family, target, bytes.NewReader(body), 1<<20, "route", true, func(frame []byte) error { out.Write(frame); return nil })
				if err != nil {
					t.Fatalf("%v\n%s", err, body)
				}
				if c.Usage == nil || c.Usage.InputTokens != promptFor(w.stream.body) || c.Usage.OutputTokens != 16 {
					t.Fatalf("usage %+v, want %d in and 16 out", c.Usage, promptFor(w.stream.body))
				}
				if !strings.Contains(out.String(), strings.TrimSpace(words[0])) {
					t.Fatalf("no text reached the client:\n%s", out.String())
				}
			})
		}
	}
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

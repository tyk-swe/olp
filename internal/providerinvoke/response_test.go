package providerinvoke

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/plugin/abi"
)

func TestResponseReaderUnwrapsForcedStreamAndRetainsUsage(t *testing.T) {
	profile, err := connectors.NewPluginProfile("digest", abi.Manifest{Name: "test", Profiles: []abi.Profile{{ID: "test-stream", Dialect: "openai-responses", Hosting: abi.Hosting{Address: "https://example.com/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}, Envelope: &abi.Envelope{Response: "result"}, ForceStreaming: true}}}}, "test-stream")
	if err != nil {
		t.Fatal(err)
	}
	reader := NewResponseReader(connectors.Config{Plugin: profile}, wire.Responses, false, 8192, 8192)
	stream := "data: " + `{"result":{"type":"response.created","response":{"id":"r","status":"in_progress","output":[]}}}` + "\n\ndata: " + `{"result":{"type":"response.completed","response":{"id":"r","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}}` + "\n\n"
	result, err := reader.Read(&http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(stream))})
	if err != nil || result.Usage == nil || result.Usage.TotalTokens != 5 || !result.UsageComplete || !strings.Contains(string(result.Body), `"status":"completed"`) || strings.Contains(string(result.Body), `"result"`) {
		t.Fatalf("forced stream lost result or usage: %+v %v", result, err)
	}
	if _, err := reader.Read(&http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}); err == nil {
		t.Fatal("accepted a non-streamed response to a forced stream")
	}
}

func TestResponseReaderBoundsUnaryBodies(t *testing.T) {
	reader := NewResponseReader(connectors.Config{}, wire.Chat, false, 8192, 2)
	if result, err := reader.Read(&http.Response{Body: io.NopCloser(strings.NewReader(`{}`))}); err != nil || string(result.Body) != `{}` {
		t.Fatalf("exact body limit refused: %+v %v", result, err)
	}
	if _, err := reader.Read(&http.Response{Body: io.NopCloser(strings.NewReader(`{} `))}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized body accepted: %v", err)
	}
}

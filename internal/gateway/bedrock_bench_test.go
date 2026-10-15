package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// BenchmarkBedrockNativeRelay includes CRC framing, bounded native observation,
// usage and delivery, with no network or downstream buffer growth.
func BenchmarkBedrockNativeRelay(b *testing.B) {
	var input bytes.Buffer
	encoder := eventstream.NewEncoder()
	native := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"message_delta","usage":{"output_tokens":7}}`,
		`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":11,"outputTokenCount":7}}`,
	}
	for _, document := range native {
		headers := eventstream.Headers{}
		headers.Set(":message-type", eventstream.StringValue("event"))
		headers.Set(":event-type", eventstream.StringValue("chunk"))
		payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(document))})
		if err := encoder.Encode(&input, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
			b.Fatal(err)
		}
	}
	s := &Server{cfg: Config{MaxEventBytes: 1 << 20}, now: time.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)), Sink: discardSink{}}
	w := &benchWriter{header: make(http.Header)}
	p := &pin{model: "anthropic.claude-3-5-sonnet-v2:0"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w.reset()
		x := &call{family: wire.BedrockInvoke, dispatched: true, request: request{startedAt: time.Now()}, facts: []AttemptFact{{Class: classSuccess}}}
		s.relayBedrockStream(context.Background(), w, x, p, &http.Response{Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(input.Bytes()))})
	}
}

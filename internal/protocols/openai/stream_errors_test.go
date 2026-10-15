package openai

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/testutil"
)

type failedStreamReader struct{ err error }

func (r failedStreamReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamClassifiesFramingWithoutChangingTransportOrClientErrors(t *testing.T) {
	for _, family := range []wire.Format{wire.Chat, wire.Responses} {
		t.Run(string(family), func(t *testing.T) {
			for _, encodedWire := range []string{"data: \xff\n\n", ": \xff\n\n", "event: \xff\n\n"} {
				_, err := Stream(family, testutil.Fragmented([]byte(encodedWire), 1), 4096, "route", true, func([]byte) error {
					t.Fatal("malformed SSE was emitted")
					return nil
				})
				var protocol *wire.ProtocolError
				if !errors.As(err, &protocol) {
					t.Fatalf("invalid UTF-8 was not a protocol error: %v", err)
				}
			}
			// Matching text in an I/O error must not turn it into a framing error.
			transport := errors.New("transport exceeds byte limit")
			if _, err := Stream(family, failedStreamReader{transport}, 4096, "route", true, func([]byte) error { return nil }); err != transport {
				t.Fatalf("transport error changed: %v", err)
			}
			encodedWire := "data: {\"choices\":[]}\n\n"
			if family == wire.Responses {
				encodedWire = "data: {\"type\":\"response.created\"}\n\n"
			}
			if _, err := Stream(family, strings.NewReader(encodedWire), 4096, "route", true, func([]byte) error { return io.ErrClosedPipe }); err != io.ErrClosedPipe {
				t.Fatalf("client write error changed: %v", err)
			}
		})
	}
}

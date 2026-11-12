package fault

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func WriteJSON(w http.ResponseWriter, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// streamWriter commits the client response on the first upstream frame and
// applies a per-frame write deadline so slow readers cannot pin upstream
// work forever.
type StreamWriter struct {
	W      http.ResponseWriter
	Family wire.Format
	// headers adds the streamed request's own headers, once as the stream is
	// committed and once as it finishes. A writer that only reports an error
	// has none.
	Headers   StreamHeaders
	Committed bool
}

type StreamHeaders interface {
	Opening(http.Header)
	Closing(h http.Header, committed bool)
}

// responseWriteTimeout bounds unary delivery and each streamed frame.
const ResponseWriteTimeout = 30 * time.Second

// errClientWrite marks a frame the client could not or would not read; the
// executor classifies it as client cancellation rather than provider failure.
var ErrClientWrite = errors.New("client write failed")

func (sw *StreamWriter) Emit(frame []byte) error {
	rc := http.NewResponseController(sw.W)
	if !sw.Committed {
		h := sw.W.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		if sw.Family == wire.Bedrock {
			h.Set("Content-Type", "application/vnd.amazon.eventstream")
		}
		h.Set("X-Accel-Buffering", "no")
		if sw.Headers != nil {
			sw.Headers.Opening(h)
		}
		sw.W.WriteHeader(http.StatusOK)
		sw.Committed = true
	}
	rc.SetWriteDeadline(time.Now().Add(ResponseWriteTimeout))
	if _, err := sw.W.Write(frame); err != nil {
		return fmt.Errorf("%w: %v", ErrClientWrite, err)
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("%w: %v", ErrClientWrite, err)
	}
	return nil
}

// finish completes the client response and returns the status it carried.
func (sw *StreamWriter) Finish(failure *Error, cancelled bool) int {
	if sw.Headers != nil {
		sw.Headers.Closing(sw.W.Header(), sw.Committed)
	}
	if failure == nil {
		return http.StatusOK
	}
	if cancelled {
		return 0 // the client is gone; nothing more can be delivered
	}
	if !sw.Committed {
		if failure.Status > 0 {
			WriteSurfaceError(sw.W, failure, sw.Family.Surface())
		}
		return failure.Status
	}
	// The response is committed: signal the failure in-band the way the
	// official SDKs detect it, then end the stream without a completion
	// marker so the client cannot mistake it for success.
	if sw.Family == wire.Bedrock {
		headers := eventstream.Headers{}
		headers.Set(":message-type", eventstream.StringValue("exception"))
		headers.Set(":exception-type", eventstream.StringValue("modelStreamErrorException"))
		payload, _ := json.Marshal(map[string]string{"message": failure.Message, "code": failure.Code})
		http.NewResponseController(sw.W).SetWriteDeadline(time.Now().Add(ResponseWriteTimeout))
		_ = eventstream.NewEncoder().Encode(sw.W, eventstream.Message{Headers: headers, Payload: payload})
		_ = http.NewResponseController(sw.W).Flush()
		return http.StatusOK
	}
	frame := "data: " + string(failure.SurfaceBody(sw.Family.Surface())) + "\n\n"
	if sw.Family == wire.Responses || sw.Family.Surface() == "anthropic" {
		frame = "event: error\n" + frame
	}
	sw.Emit([]byte(frame))
	return http.StatusOK
}

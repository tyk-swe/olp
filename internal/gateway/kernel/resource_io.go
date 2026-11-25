package kernel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) ReadResourceBody(w http.ResponseWriter, r *http.Request) ([]byte, *fault.Error) {
	deadline := time.Now().Add(RequestBodyTimeout)
	if parent, ok := r.Context().Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(deadline); err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request body could not be bounded.")
	}
	body, failure := s.Gate.ReadBody(r)
	if failure == nil {
		_ = controller.SetReadDeadline(time.Time{})
	}
	// Failed reads keep their deadline while HTTP/1 drains the remaining body.
	return body, failure
}

func SetFileDownloadHeaders(h http.Header, contentType string, contentLength int64) {
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.FormatInt(contentLength, 10))
	h.Set("Content-Disposition", "attachment")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
}

func (s *Server) DeliverResourceContent(ctx context.Context, w http.ResponseWriter, x *Call, exchange *PinnedExchange, reader io.Reader, expected int64, failureMessage string) {
	x.ResponseHeaders(w.Header(), false)
	w.WriteHeader(http.StatusOK)
	written, err := io.Copy(w, reader)
	if err != nil || written != expected || http.NewResponseController(w).Flush() != nil {
		exchange.Delivery(DeliveryState(written))
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", failureMessage)
		s.finishContext(ctx, x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, x.Failure.Status)
		return
	}
	if written > 0 {
		x.Delivered(s.Now())
	}
	exchange.Delivery(usage.ClientTerminal)
	s.finishContext(ctx, x, &Outcome{Committed: true}, http.StatusOK)
}

func MustJSON(value any) []byte {
	out, _ := json.Marshal(value)
	return out
}

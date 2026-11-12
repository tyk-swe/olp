package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/fault"
)

func (s *Server) readResourceBody(w http.ResponseWriter, r *http.Request) ([]byte, *fault.Error) {
	deadline := time.Now().Add(requestBodyTimeout)
	if parent, ok := r.Context().Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(deadline); err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request body could not be bounded.")
	}
	body, failure := s.readBody(r)
	if failure == nil {
		_ = controller.SetReadDeadline(time.Time{})
	}
	// Failed reads keep their deadline while HTTP/1 drains the remaining body.
	return body, failure
}

func copyResourceContent(w http.ResponseWriter, reader io.Reader, expected int64) (int64, error) {
	written, err := io.Copy(w, reader)
	if err == nil && written != expected {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = http.NewResponseController(w).Flush()
	}
	return written, err
}

func mustJSON(value any) []byte {
	out, _ := json.Marshal(value)
	return out
}

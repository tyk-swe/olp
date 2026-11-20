package kernel

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/fault"
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

func CopyResourceContent(w http.ResponseWriter, reader io.Reader, expected int64) (int64, error) {
	written, err := io.Copy(w, reader)
	if err == nil && written != expected {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = http.NewResponseController(w).Flush()
	}
	return written, err
}

func MustJSON(value any) []byte {
	out, _ := json.Marshal(value)
	return out
}

package gateway

import (
	"net/http"
	"testing"
	"time"
)

const socketTestTimeout = 250 * time.Millisecond

// Keep real socket reads and writes, but check the production deadline before
// shortening it. Clearing a deadline still reaches net/http unchanged.
type socketDeadlineWriter struct {
	http.ResponseWriter
	t *testing.T
}

func (w socketDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w socketDeadlineWriter) shorten(deadline time.Time, want time.Duration) time.Time {
	if deadline.IsZero() {
		return deadline
	}
	if remaining := time.Until(deadline); remaining < want-time.Second || remaining > want {
		w.t.Errorf("requested socket deadline in %s; want %s", remaining, want)
	}
	return time.Now().Add(socketTestTimeout)
}

func (w socketDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(w.shorten(deadline, requestBodyTimeout))
}

func (w socketDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.shorten(deadline, responseWriteTimeout))
}

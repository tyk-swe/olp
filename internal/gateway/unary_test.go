package gateway

import (
	"net/http/httptest"
	"time"
)

type unaryResponseWriter struct {
	*httptest.ResponseRecorder
	writeErr, flushErr error
	deadline           time.Time
	beforeDelivery     func()
}

func (w *unaryResponseWriter) SetReadDeadline(time.Time) error { return nil }
func (w *unaryResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *unaryResponseWriter) Write(p []byte) (int, error) {
	w.beforeDelivery()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(p)
}
func (w *unaryResponseWriter) FlushError() error {
	w.beforeDelivery()
	return w.flushErr
}

// A small send buffer stalls delivery without spending the failure guard
// serializing megabytes under the race detector.

// Keep the connection open without reading any of the large response.

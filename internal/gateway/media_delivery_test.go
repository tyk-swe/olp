package gateway

import (
	"net/http/httptest"
	"time"
)

type mediaDeadlineWriter struct {
	*httptest.ResponseRecorder
	readDeadline time.Time
}

func (w *mediaDeadlineWriter) SetReadDeadline(d time.Time) error { w.readDeadline = d; return nil }
func (w *mediaDeadlineWriter) SetWriteDeadline(time.Time) error  { return nil }

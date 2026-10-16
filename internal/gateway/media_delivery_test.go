package gateway

import (
	"io"
	"net/http/httptest"
	"time"
)

type mediaDeadlineWriter struct {
	*httptest.ResponseRecorder
	readDeadline time.Time
}

func (w *mediaDeadlineWriter) SetReadDeadline(d time.Time) error { w.readDeadline = d; return nil }
func (w *mediaDeadlineWriter) SetWriteDeadline(time.Time) error  { return nil }

type checkedMediaBody struct {
	io.ReadCloser
	check func()
	err   error
}

func (b checkedMediaBody) Read(p []byte) (int, error) {
	b.check()
	if b.err != nil {
		return 0, b.err
	}
	return b.ReadCloser.Read(p)
}

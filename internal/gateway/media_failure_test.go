package gateway

import (
	"io"
	"net/http"
	"time"
)

type mediaSinkFunc func(Envelope)

func (f mediaSinkFunc) Terminal(e Envelope) { f(e) }

type failedMediaWriter struct{ header http.Header }

func (w *failedMediaWriter) Header() http.Header              { return w.header }
func (w *failedMediaWriter) WriteHeader(int)                  {}
func (w *failedMediaWriter) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (w *failedMediaWriter) Flush()                           {}
func (w *failedMediaWriter) SetReadDeadline(time.Time) error  { return nil }
func (w *failedMediaWriter) SetWriteDeadline(time.Time) error { return nil }

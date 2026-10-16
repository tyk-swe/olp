//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Gate the actual original delivery after native acceptance and resource
// publication. Concurrent financial observers can then win the resource lock
// before finish supplies the original HTTP outcome.
type deferredDeliveryGate struct {
	header           http.Header
	status           int
	body             bytes.Buffer
	offered          []byte
	entered, release chan struct{}
	once             sync.Once
	fail             bool
	cancelRequest    context.CancelFunc
}

func (w *deferredDeliveryGate) Header() http.Header              { return w.header }
func (w *deferredDeliveryGate) WriteHeader(status int)           { w.status = status }
func (w *deferredDeliveryGate) SetReadDeadline(time.Time) error  { return nil }
func (w *deferredDeliveryGate) SetWriteDeadline(time.Time) error { return nil }
func (w *deferredDeliveryGate) Flush()                           {}
func (w *deferredDeliveryGate) Write(body []byte) (int, error) {
	w.once.Do(func() {
		w.offered = append([]byte(nil), body...)
		close(w.entered)
	})
	<-w.release
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.body.Write(body)
}

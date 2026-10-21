//go:build bench

package mockupstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

// discard is a response writer that keeps nothing, so a measurement sees the
// handler's own cost.
type discard struct {
	header http.Header
	bytes  int
}

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Flush()                      {}
func (d *discard) Write(p []byte) (int, error) { d.bytes += len(p); return len(p), nil }

// replay is a request body that can be read again.
type replay struct{ *bytes.Reader }

func (replay) Close() error { return nil }

// harness serves one request repeatedly without building a new one each time.
type harness struct {
	h   http.Handler
	req *http.Request
	rep replay
	w   *discard
}

func newHarness(tb testing.TB, s *Server, r request) *harness {
	tb.Helper()
	u, err := url.Parse("http://mock" + r.path)
	if err != nil {
		tb.Fatal(err)
	}
	rep := replay{bytes.NewReader([]byte(r.body))}
	req := &http.Request{Method: http.MethodPost, URL: u, Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(r.body))}}, Body: rep}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	return &harness{h: s, req: req.WithContext(context.Background()), rep: rep, w: &discard{header: http.Header{}}}
}

func (h *harness) serve() {
	h.rep.Seek(0, io.SeekStart)
	h.req.Body = h.rep
	clear(h.w.header)
	h.w.bytes = 0
	h.h.ServeHTTP(h.w, h.req)
}

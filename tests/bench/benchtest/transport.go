//go:build bench

// Package benchtest holds what the benchmark packages' tests share.
package benchtest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// HandlerTransport is an http.RoundTripper that serves requests by calling a
// handler in a new goroutine, without a network. Inside a testing/synctest
// bubble it makes whole client and server exchanges run on the fake clock, so
// a test can ask for a three-second stall and wait no time at all.
//
// The response body is an unbuffered pipe, so a handler that writes faster
// than the client reads blocks, as it would on a full socket, and a handler
// that panics with http.ErrAbortHandler truncates the body with
// io.ErrUnexpectedEOF, as an aborted connection does.
type HandlerTransport struct{ Handler http.Handler }

func (t HandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		closeBody(req)
		return nil, err
	}
	served := req.Clone(ctx)
	served.RequestURI = req.URL.RequestURI()
	if req.Body == nil {
		served.Body = http.NoBody
	}
	pr, pw := io.Pipe()
	w := &responseWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		defer closeBody(req)
		defer func() {
			switch p := recover(); p {
			case nil:
				w.commit()
				pw.Close()
			case http.ErrAbortHandler:
				w.commit()
				pw.CloseWithError(io.ErrUnexpectedEOF)
			default:
				w.commit()
				pw.CloseWithError(fmt.Errorf("handler panic: %v", p))
			}
		}()
		t.Handler.ServeHTTP(w, served)
	}()
	select {
	case <-w.ready:
	case <-ctx.Done():
		pr.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}
	// A canceled request tears the connection down, which fails a handler
	// blocked writing to it as well as one asleep.
	stop := context.AfterFunc(ctx, func() { pr.CloseWithError(ctx.Err()) })
	resp := &http.Response{
		Status:     strconv.Itoa(w.status) + " " + http.StatusText(w.status),
		StatusCode: w.status,
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        w.snapshot,
		Body:          body{pr, ctx, stop},
		ContentLength: -1,
		Request:       req,
	}
	if n, err := strconv.ParseInt(w.snapshot.Get("Content-Length"), 10, 64); err == nil {
		resp.ContentLength = n
	}
	return resp, nil
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}

// body reports the request's cancellation instead of a clean end when a read
// fails after the context is done, as a real transport does: a handler woken
// by the same cancellation closes the pipe, which would otherwise look like a
// complete response.
type body struct {
	*io.PipeReader
	ctx  context.Context
	stop func() bool
}

func (b body) Read(p []byte) (int, error) {
	n, err := b.PipeReader.Read(p)
	if err != nil {
		if cause := b.ctx.Err(); cause != nil {
			err = cause
		}
	}
	return n, err
}

func (b body) Close() error {
	b.stop()
	return b.PipeReader.CloseWithError(io.ErrClosedPipe)
}

// responseWriter is the server side of the pipe. Headers are fixed, and the
// response released to the client, at the first WriteHeader, Write or Flush.
type responseWriter struct {
	header   http.Header
	pw       *io.PipeWriter
	status   int
	snapshot http.Header
	ready    chan struct{}
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	if w.snapshot != nil {
		return
	}
	w.status = status
	w.snapshot = w.header.Clone()
	close(w.ready)
}

func (w *responseWriter) commit() { w.WriteHeader(http.StatusOK) }

func (w *responseWriter) Write(p []byte) (int, error) {
	w.commit()
	return w.pw.Write(p)
}

func (w *responseWriter) Flush() { w.commit() }

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Carry hands the unconfined plugin with digest one finished upstream request
// for provider, of a profile whose traffic the plugin carries, and returns the
// upstream's response as the plugin streams it back: its head once the plugin
// sends it, and its body as each part arrives, so a stream's events reach the
// caller as the upstream sends them. It implements connectors.Carrier.
//
// A failure wraps connectors.ErrNotSent when the request never reached the
// upstream: OLP could not hand it to the plugin, or the plugin reported that
// it did not send it. After any other failure, including ctx ending before the
// plugin answered, the upstream's outcome is unknown. Ending ctx or closing the
// response's body cancels the call if the plugin still streams the response,
// and the plugin then answers it within the time limit. The Host keeps the
// plugin's code until the caller closes the body.
func (h *Host) Carry(ctx context.Context, digest string, provider abi.Provider, req *http.Request, secrets []string) (*http.Response, error) {
	request, err := carriedRequest(req)
	if err != nil {
		return nil, notSent(err)
	}
	entry := h.use(digest)
	call, cancel := context.WithCancel(ctx)
	invocation := Call{Method: abi.MethodCarry, Params: request, Provider: &provider, Secrets: secrets}
	invocation.out = invocation.output(h.runtime.log, digest)
	end := sync.OnceFunc(func() {
		cancel()
		h.done(entry)
	})
	head, result, err := h.carry(call, entry, invocation)
	if err != nil {
		end()
		h.failed(ctx, invocation.out, abi.MethodCarry, err)
		return nil, err
	}
	header := http.Header{}
	for name, values := range head.Header {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	body := &carriedBody{result: result, held: head.Body, end: end, failed: func(err error) { h.failed(call, invocation.out, abi.MethodCarry, err) }}
	return &http.Response{
		Status: fmt.Sprintf("%d %s", head.Status, http.StatusText(head.Status)), StatusCode: head.Status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: header, Body: body, ContentLength: -1, Request: req,
	}, nil
}

// carriedRequest reads the finished request a plugin carries, closing its
// body.
func carriedRequest(req *http.Request) (abi.HTTPRequest, error) {
	request := abi.HTTPRequest{Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone()}
	if req.Body == nil {
		return request, nil
	}
	defer req.Body.Close()
	var err error
	request.Body, err = io.ReadAll(req.Body)
	return request, err
}

// carry hands the plugin the request call carries and waits for the head of
// the upstream's response.
func (h *Host) carry(ctx context.Context, entry *hosted, call Call) (abi.HTTPResponse, *streamed, error) {
	var head abi.HTTPResponse
	loaded, err := entry.wait(ctx)
	if err != nil {
		return head, nil, notSent(err)
	}
	executable, ok := loaded.(*Executable)
	if !ok {
		return head, nil, notSent(refuse(CodeFailed, "A confined plugin carries no traffic."))
	}
	result, err := executable.stream(ctx, call)
	if err != nil {
		return head, nil, notSent(err)
	}
	part, err := result.next()
	if reported, ok := errors.AsType[*abi.Error](err); ok && reported.Code == abi.CodeNotSent {
		return head, nil, notSent(err)
	}
	switch {
	case errors.Is(err, io.EOF):
		return head, nil, refuse(CodeFailed, "The plugin answered a carried request without the upstream's response.")
	case err != nil:
		return head, nil, err
	case json.Unmarshal(part, &head) != nil || head.Status < 100 || head.Status > 599:
		return head, nil, refuse(CodeFailed, "The plugin's response to a carried request does not start with its status.")
	}
	return head, result, nil
}

// carriedBody is the body of a response a plugin carries: the bytes of the
// parts it streams after the response's head.
type carriedBody struct {
	result *streamed
	// held is what the body holds of the last part read.
	held []byte
	err  error
	// end cancels the call, if it still runs, and releases the plugin's code.
	end    func()
	failed func(error)
}

func (b *carriedBody) Read(p []byte) (int, error) {
	for len(b.held) == 0 && len(p) > 0 {
		if b.err != nil {
			return 0, b.err
		}
		part, err := b.result.next()
		var more abi.HTTPResponse
		switch {
		case errors.Is(err, io.EOF):
			b.err = io.EOF
		case err != nil:
			b.err = err
			b.failed(err)
		case json.Unmarshal(part, &more) != nil || more.Status != 0 || more.Header != nil:
			b.err = refuse(CodeFailed, "The plugin broke off a carried response: a part after its head holds more than body bytes.")
			b.failed(b.err)
			b.end()
		default:
			b.held = more.Body
		}
	}
	n := copy(p, b.held)
	b.held = b.held[n:]
	return n, nil
}

// Close cancels the call, if the plugin still streams the response.
func (b *carriedBody) Close() error {
	b.end()
	return nil
}

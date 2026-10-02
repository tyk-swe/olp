// Package codexfixture is a controlled wire peer, never evidence of subscription eligibility.
package codexfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

const SSE = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-controlled\"}}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"CONTROLLED_PRIVATE_OUTPUT\"}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-controlled\",\"content\":[{\"type\":\"output_text\",\"text\":\"CONTROLLED_PRIVATE_OUTPUT\"}]}}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-controlled\",\"usage\":{\"input_tokens\":12,\"output_tokens\":4,\"total_tokens\":16,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n"

type Request struct {
	Path      string
	Header    http.Header
	Body      []byte
	WebSocket bool
}

// Captures are memory-only and contain synthetic values. Do not point this peer
// at real user traffic or serialize its captures into application diagnostics.
type Upstream struct {
	mu              sync.Mutex
	requests        []Request
	mode            string
	faults          []string
	handshakeStatus int
	handshakes      []http.Header
	responder       func(context.Context, http.Header, []byte) [][]byte
	Canceled        chan struct{}
}

func New() *Upstream { return &Upstream{Canceled: make(chan struct{}, 32)} }

func (u *Upstream) Faults(faults ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.faults = append([]string(nil), faults...)
}

func (u *Upstream) RejectWebSockets(status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handshakeStatus = status
}

func (u *Upstream) Handshakes() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	result := make([]http.Header, 0, len(u.handshakes))
	for _, header := range u.handshakes {
		result = append(result, header.Clone())
	}
	return result
}

func (u *Upstream) SetResponder(responder func(context.Context, http.Header, []byte) [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.responder = responder
}

func (u *Upstream) reply(r *http.Request, body []byte) [][]byte {
	u.mu.Lock()
	responder := u.responder
	u.mu.Unlock()
	if responder == nil {
		return nil
	}
	return responder(r.Context(), r.Header, body)
}

func (u *Upstream) Mode(mode string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.mode = mode
}

func (u *Upstream) Requests() []Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	result := make([]Request, 0, len(u.requests))
	for _, request := range u.requests {
		result = append(result, Request{request.Path, request.Header.Clone(), bytes.Clone(request.Body), request.WebSocket})
	}
	return result
}

func (u *Upstream) record(r *http.Request, body []byte, ws bool) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, Request{r.URL.RequestURI(), r.Header.Clone(), bytes.Clone(body), ws})
	var request struct {
		Generate *bool `json:"generate"`
	}
	_ = json.Unmarshal(body, &request)
	if len(u.faults) != 0 && (request.Generate == nil || *request.Generate) {
		fault := u.faults[0]
		u.faults = u.faults[1:]
		return fault
	}
	return u.mode
}

func (u *Upstream) canceled() {
	select {
	case u.Canceled <- struct{}{}:
	default:
	}
}

func (u *Upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		u.mu.Lock()
		u.handshakes = append(u.handshakes, r.Header.Clone())
		status := u.handshakeStatus
		u.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"code":"controlled_websocket_unsupported"}}`)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(4 << 20)
		for {
			_, body, err := conn.Read(r.Context())
			if err != nil {
				u.canceled()
				return
			}
			mode := u.record(r, body, true)
			if mode == "disconnect" {
				return
			}
			if events := u.reply(r, body); events != nil {
				for _, event := range events {
					if err := conn.Write(r.Context(), websocket.MessageText, event); err != nil {
						return
					}
				}
				continue
			}
			var request struct {
				Generate *bool `json:"generate"`
			}
			if json.Unmarshal(body, &request) == nil && request.Generate != nil && !*request.Generate {
				if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp-prewarm"}}`)); err != nil {
					return
				}
				continue
			}
			if mode == "hold" {
				_, _, _ = conn.Read(r.Context())
				u.canceled()
				return
			}
			if mode == "unavailable" {
				_ = conn.Close(websocket.StatusTryAgainLater, "controlled unavailable")
				return
			}
			for _, line := range strings.Split(SSE, "\n") {
				if event, ok := strings.CutPrefix(line, "data: "); ok {
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
						return
					}
				}
			}
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return
	}
	mode := u.record(r, body, false)
	if mode == "unavailable" {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"code":"controlled_unavailable"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Add("X-Controlled-Multi", "first")
	w.Header().Add("X-Controlled-Multi", "second")
	w.Header().Set("X-Request-Id", "controlled-upstream-request")
	w.WriteHeader(200)
	if mode == "hold" || mode == "disconnect" {
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-controlled\"}}\n\n")
		w.(http.Flusher).Flush()
		if mode == "hold" {
			<-r.Context().Done()
			u.canceled()
		}
		return
	}
	if events := u.reply(r, body); events != nil {
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
			w.(http.Flusher).Flush()
		}
		return
	}
	fmt.Fprint(w, SSE)
	w.(http.Flusher).Flush()
}

func ReadGeneration(ctx context.Context, conn *websocket.Conn) ([][]byte, error) {
	var events [][]byte
	for {
		_, event, err := conn.Read(ctx)
		if err != nil {
			return events, err
		}
		events = append(events, event)
		if bytes.Contains(event, []byte(`"response.completed"`)) {
			return events, nil
		}
	}
}

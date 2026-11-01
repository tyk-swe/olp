package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcpservers"
)

type sessionTransport string

const (
	sessionStreamable sessionTransport = "streamable_http"
	sessionLegacy     sessionTransport = "sse"
)

type legacyStream struct {
	negotiated atomic.Bool
	frames     chan []byte
	activity   chan struct{}
	done       chan struct{}
}

type legacyResponse struct {
	writer http.ResponseWriter
	ctx    context.Context
	stream *legacyStream
}

func (w legacyResponse) Unwrap() http.ResponseWriter { return w.writer }

func (w legacyResponse) Header() http.Header { return make(http.Header) }
func (w legacyResponse) WriteHeader(int)     {}
func (w legacyResponse) Write(body []byte) (int, error) {
	select {
	case w.stream.frames <- bytes.Clone(body):
		return len(body), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	case <-w.stream.done:
		return 0, context.Canceled
	}
}

func (s *Server) serveLegacyGET(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeRPCFailure(w, nil, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	record, refused := s.createSessionRecord(r, authority, mcpservers.LegacyProtocol)
	if refused != nil {
		writeRPCFailure(w, nil, *refused)
		return
	}
	stream := &legacyStream{frames: make(chan []byte, 1), activity: make(chan struct{}, 1), done: make(chan struct{})}
	s.legacyStreams.Store(record.id, stream)
	defer s.closeLegacyStream(r.Context(), record, stream)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	if writeLegacyEvent(w, "endpoint", []byte("/mcp/messages?session_id="+record.id)) != nil {
		return
	}
	timer := time.NewTimer(sessionIdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			return
		case <-stream.activity:
			timer.Reset(sessionIdleTimeout)
		case frame := <-stream.frames:
			if writeLegacyEvent(w, "message", frame) != nil {
				return
			}
		}
	}
}

func writeLegacyEvent(w http.ResponseWriter, event string, body []byte) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(defaultCallTimeout)); err != nil {
		return err
	}
	defer controller.SetWriteDeadline(time.Time{})
	lines := bytes.ReplaceAll(bytes.TrimSpace(body), []byte("\n"), []byte("\ndata: "))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, lines); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

func (s *Server) closeLegacyStream(ctx context.Context, record sessionRecord, stream *legacyStream) {
	s.legacyStreams.Delete(record.id)
	close(stream.done)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = s.Sessions.deleteSession(cleanup, record.id, record.binding)
}

func (s *Server) serveLegacyPOST(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticateSession(r)
	if failure != nil {
		writeRPCFailure(w, nil, failureRPC(failure.status, rpcServerError, failure))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), defaultCallTimeout)
	defer cancel()
	request := r.Clone(ctx)
	request.Header.Set("Mcp-Session-Id", r.URL.Query().Get("session_id"))
	record, _, refused := s.requireSession(request.Context(), request, authority, sessionLegacy)
	if refused != nil {
		writeRPCFailure(w, nil, *refused)
		return
	}
	value, exists := s.legacyStreams.Load(record.id)
	if !exists {
		writeRPCFailure(w, nil, failureRPC(http.StatusNotFound, rpcInvalidRequest, errSessionNotFound))
		return
	}
	stream := value.(*legacyStream)
	select {
	case <-stream.done:
		writeRPCFailure(w, nil, failureRPC(http.StatusNotFound, rpcInvalidRequest, errSessionNotFound))
		return
	default:
	}
	select {
	case stream.activity <- struct{}{}:
	default:
	}
	deadline, _ := ctx.Deadline()
	if err := http.NewResponseController(w).SetReadDeadline(deadline); err != nil {
		writeRPCFailure(w, nil, rpcFailure{status: http.StatusInternalServerError, code: rpcServerError, message: "The request body could not be bounded.", data: rpcErrorData{Code: "read_deadline_unavailable"}})
		return
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
	s.serveSessionHTTP(legacyResponse{writer: w, ctx: ctx, stream: stream}, request, sessionLegacy)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) initializeLegacySession(w http.ResponseWriter, r *http.Request, authority access.Authority, message rpcMessage) {
	var params initializeRequest
	if json.Unmarshal(message.params, &params) != nil || params.ProtocolVersion != mcpservers.LegacyProtocol {
		writeRPCFailure(w, message.id, rpcFailure{status: http.StatusBadRequest, code: rpcInvalidParams, message: "Legacy HTTP+SSE requires protocol version 2024-11-05.", data: rpcErrorData{Code: "invalid_params"}})
		return
	}
	record, _, refused := s.requireSession(r.Context(), r, authority, sessionLegacy)
	if refused != nil {
		writeRPCFailure(w, message.id, *refused)
		return
	}
	stream, exists := s.legacyStreams.Load(record.id)
	if !exists {
		writeRPCFailure(w, message.id, failureRPC(http.StatusNotFound, rpcInvalidRequest, errSessionNotFound))
		return
	}
	stream.(*legacyStream).negotiated.Store(true)
	writeInitializeResult(w, message.id, record)
}

func (s *Server) legacyNegotiated(sessionID string) bool {
	stream, exists := s.legacyStreams.Load(sessionID)
	return exists && stream.(*legacyStream).negotiated.Load()
}

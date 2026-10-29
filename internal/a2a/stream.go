package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/runtime"
)

var errStreamEventSize = errors.New("stream event exceeds one MiB")

// Event framing must persist across reads because network chunks can split SSE lines.
type boundedEventBody struct {
	io.ReadCloser
	size    int
	hasLine bool
}

func (b *boundedEventBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	for i, value := range p[:n] {
		b.size++
		if b.size > maxRPCBytes {
			return i, errStreamEventSize
		}
		switch value {
		case '\n':
			if !b.hasLine {
				b.size = 0
			}
			b.hasLine = false
		case '\r':
		default:
			b.hasLine = true
		}
	}
	return n, err
}

func (s *Server) streamContext(r *http.Request, authority access.Authority, revision agentcontract.Revision) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.streamAuthorized(r, authority, revision) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func (s *Server) streamAuthorized(r *http.Request, authority access.Authority, revision agentcontract.Revision) bool {
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	current, failure := consumerauth.Authenticate(s.Runtime, r, token, "agents", s.TrustedProxies, time.Now())
	return failure == nil && current.ID == authority.ID && s.Runtime.AuthorizeAgentResource(current, "agents", revision.PublicationID, revision.ID, revision.ProjectID) == nil
}

func (s *Server) sendStream(w http.ResponseWriter, r *http.Request, authority access.Authority, revision agentcontract.Revision, release *runtime.Release, request rpcRequest) {
	if _, ok := w.(http.Flusher); !ok {
		fail(w, request.ID, -32603, "Streaming unavailable")
		return
	}
	prepared, failure := s.prepareSend(r.Context(), authority, revision, release, request.Params)
	if failure != nil {
		reply(w, request.ID, nil, failure)
		return
	}
	card, err := s.loadCard(r.Context(), *prepared.Revision.Definition.Agent)
	if err != nil || !card.Capabilities.Streaming {
		fail(w, request.ID, -32603, "Streaming unavailable")
		return
	}
	transport, headers, cleanup, err := s.transport(r.Context(), *prepared.Revision.Definition.Agent)
	if err != nil {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	defer cleanup()
	res, err := s.Ledger.reserve(r.Context(), authority, prepared.Revision, release)
	if err != nil {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	if !s.streamAuthorized(r, authority, prepared.Revision) {
		s.Ledger.release(r.Context(), res)
		fail(w, request.ID, -32603, "Authorization revoked")
		return
	}
	if err := s.Ledger.Store.MarkDispatched(r.Context(), res.DispatchID, res.ClaimID); err != nil {
		s.Ledger.release(r.Context(), res)
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	ctx, cancel := s.streamContext(r, authority, prepared.Revision)
	defer cancel()
	next, stop := iter.Pull2(transport.SendStreamingMessage(ctx, headers, &prepared.Params))
	defer stop()
	first, eventErr, ok := next()
	if !ok || eventErr != nil || first == nil {
		s.Ledger.unknown(r.Context(), res)
		fail(w, request.ID, -32603, streamError(eventErr))
		return
	}
	info := first.TaskInfo()
	if !validInitialStreamEvent(first, prepared.ExpectedUpstreamID != "") || prepared.ExpectedUpstreamID != "" && !streamEventMatches(first, prepared.ExpectedUpstreamID, prepared.Params.Message.ContextID) {
		s.Ledger.unknown(r.Context(), res)
		fail(w, request.ID, -32603, "Invalid upstream stream")
		return
	}
	settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	publicID, settleErr := s.Ledger.settle(settleCtx, res, authority, prepared.Revision, string(info.TaskID), info.ContextID)
	settleCancel()
	if settleErr != nil {
		s.Ledger.unknown(r.Context(), res)
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	if !s.streamAuthorized(r, authority, prepared.Revision) {
		fail(w, request.ID, -32603, "Authorization revoked")
		return
	}
	streamHeaders(w)
	if !writeStreamEvent(w, request.ID, first, publicID) {
		return
	}
	s.forwardStream(w, r, authority, prepared.Revision, request.ID, string(info.TaskID), info.ContextID, publicID, next)
}

func validInitialStreamEvent(event protocol.Event, continuation bool) bool {
	switch value := event.(type) {
	case *protocol.Task:
		return value != nil && value.ID != "" && value.ContextID != ""
	case *protocol.Message:
		return value != nil && value.ID != "" && (value.TaskID == "") == (value.ContextID == "")
	case *protocol.TaskStatusUpdateEvent:
		return continuation && value != nil && value.TaskID != "" && value.ContextID != ""
	case *protocol.TaskArtifactUpdateEvent:
		return continuation && value != nil && value.TaskID != "" && value.ContextID != ""
	default:
		return false
	}
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request, authority access.Authority, revision agentcontract.Revision, release *runtime.Release, request rpcRequest) {
	if _, ok := w.(http.Flusher); !ok {
		fail(w, request.ID, -32603, "Streaming unavailable")
		return
	}
	var params protocol.SubscribeToTaskRequest
	if json.Unmarshal(request.Params, &params) != nil || params.ID == "" || params.Tenant != "" {
		fail(w, request.ID, -32602, "Invalid parameters")
		return
	}
	owned, err := s.Ledger.findTask(r.Context(), authority, revision.PublicationID, string(params.ID))
	if err != nil {
		fail(w, request.ID, -32001, "Task not found")
		return
	}
	pinned, ok := s.pinnedRevision(authority, revision, owned, release)
	if !ok {
		fail(w, request.ID, -32001, "Task not found")
		return
	}
	card, err := s.loadCard(r.Context(), *pinned.Definition.Agent)
	if err != nil || !card.Capabilities.Streaming {
		fail(w, request.ID, -32603, "Streaming unavailable")
		return
	}
	transport, headers, cleanup, err := s.transport(r.Context(), *pinned.Definition.Agent)
	if err != nil {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	defer cleanup()
	if !s.streamAuthorized(r, authority, pinned) {
		fail(w, request.ID, -32603, "Authorization revoked")
		return
	}
	ctx, cancel := s.streamContext(r, authority, pinned)
	defer cancel()
	next, stop := iter.Pull2(transport.SubscribeToTask(ctx, headers, &protocol.SubscribeToTaskRequest{ID: protocol.TaskID(owned.UpstreamID)}))
	defer stop()
	first, eventErr, ok := next()
	if !ok || eventErr != nil || first == nil {
		fail(w, request.ID, -32603, streamError(eventErr))
		return
	}
	if !streamEventMatches(first, owned.UpstreamID, owned.ContextID) || !s.streamAuthorized(r, authority, pinned) {
		fail(w, request.ID, -32603, "Invalid upstream stream")
		return
	}
	streamHeaders(w)
	if !writeStreamEvent(w, request.ID, first, owned.ID) {
		return
	}
	s.forwardStream(w, r, authority, pinned, request.ID, owned.UpstreamID, owned.ContextID, owned.ID, next)
}

func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
}

func (s *Server) forwardStream(w http.ResponseWriter, r *http.Request, authority access.Authority, revision agentcontract.Revision, id json.RawMessage, upstreamID, contextID, publicID string, next func() (protocol.Event, error, bool)) {
	for {
		event, err, ok := next()
		if !ok {
			return
		}
		if err != nil {
			if !s.streamAuthorized(r, authority, revision) {
				writeStreamFailure(w, id, "Authorization revoked")
				return
			}
			writeStreamFailure(w, id, streamError(err))
			return
		}
		if event == nil || !streamEventMatches(event, upstreamID, contextID) {
			writeStreamFailure(w, id, "Invalid upstream stream")
			return
		}
		if !s.streamAuthorized(r, authority, revision) {
			writeStreamFailure(w, id, "Authorization revoked")
			return
		}
		if !writeStreamEvent(w, id, event, publicID) {
			return
		}
	}
}

func streamEventMatches(event protocol.Event, upstreamID, contextID string) bool {
	info := event.TaskInfo()
	return string(info.TaskID) == upstreamID && info.ContextID == contextID
}

func streamError(err error) string {
	if errors.Is(err, errStreamEventSize) {
		return "Stream event exceeds one MiB"
	}
	return "Internal error"
}

func writeStreamEvent(w http.ResponseWriter, id json.RawMessage, event protocol.Event, publicID string) bool {
	public := publicStreamEvent(event, publicID)
	result, err := json.Marshal(protocol.StreamResponse{Event: public})
	if err != nil || public == nil {
		writeStreamFailure(w, id, "Invalid upstream stream")
		return false
	}
	response, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: responseID(id), Result: result})
	if err != nil || len(response)+8 > maxRPCBytes {
		writeStreamFailure(w, id, "Stream event exceeds one MiB")
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", response); err != nil {
		return false
	}
	w.(http.Flusher).Flush()
	return true
}

func writeStreamFailure(w http.ResponseWriter, id json.RawMessage, message string) {
	response, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: responseID(id), Error: &rpcError{Code: -32603, Message: message}})
	if _, err := fmt.Fprintf(w, "data: %s\n\n", response); err == nil {
		w.(http.Flusher).Flush()
	}
}

func publicStreamEvent(event protocol.Event, id string) protocol.Event {
	switch value := event.(type) {
	case *protocol.Task:
		return publicTask(value, id)
	case *protocol.Message:
		return publicMessage(value, id)
	case *protocol.TaskStatusUpdateEvent:
		return &protocol.TaskStatusUpdateEvent{TaskID: protocol.TaskID(id), ContextID: id, Metadata: value.Metadata, Status: protocol.TaskStatus{State: value.Status.State, Message: publicMessage(value.Status.Message, id), Timestamp: value.Status.Timestamp}}
	case *protocol.TaskArtifactUpdateEvent:
		return &protocol.TaskArtifactUpdateEvent{TaskID: protocol.TaskID(id), ContextID: id, Artifact: value.Artifact, Append: value.Append, LastChunk: value.LastChunk, Metadata: value.Metadata}
	default:
		return nil
	}
}

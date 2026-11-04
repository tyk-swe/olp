package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"mime"
	"net/http"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/release"
)

const maxRPCBytes = 1 << 20

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(w http.ResponseWriter, id json.RawMessage, result json.RawMessage, failure *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: responseID(id), Result: result, Error: failure})
}

func responseID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func validRequestID(id json.RawMessage) bool {
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return false
	}
	var stringID string
	if json.Unmarshal(id, &stringID) == nil {
		return true
	}
	var numberID json.Number
	return json.Unmarshal(id, &numberID) == nil && numberID != ""
}

func fail(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	reply(w, id, nil, &rpcError{Code: code, Message: message})
}

func (s *Server) rpc(w http.ResponseWriter, r *http.Request) {
	authority, revision, snapshot, status := s.resolve(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	version := requestProtocol(r)
	if version == "" {
		http.Error(w, "Unsupported A2A version", http.StatusBadRequest)
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		http.Error(w, "Unsupported binding", http.StatusUnsupportedMediaType)
		return
	}
	body, bodyErr := io.ReadAll(io.LimitReader(r.Body, maxRPCBytes+1))
	if bodyErr != nil || len(body) > maxRPCBytes {
		fail(w, nil, -32600, "Request payload validation error")
		return
	}
	var request rpcRequest
	if json.Unmarshal(body, &request) != nil {
		fail(w, nil, -32700, "Invalid JSON payload")
		return
	}
	if !validRequestID(request.ID) {
		fail(w, nil, -32600, "Request payload validation error")
		return
	}
	if request.JSONRPC != "2.0" || request.Method == "" || len(request.Params) == 0 {
		fail(w, request.ID, -32600, "Request payload validation error")
		return
	}
	decoded, decodeErr := decodeRequest(version, request)
	if decodeErr != nil {
		if version != revision.Definition.Agent.Protocol {
			http.Error(w, "Unsupported A2A version", http.StatusBadRequest)
			return
		}
		if errors.Is(decodeErr, errMethodNotDeclared) {
			fail(w, request.ID, -32601, "Method not found")
			return
		}
		fail(w, request.ID, -32602, "Invalid parameters")
		return
	}
	if send, ok := decoded.(sendCommand); ok && version != revision.Definition.Agent.Protocol && (send.params.Message == nil || send.params.Message.TaskID == "") {
		http.Error(w, "Unsupported A2A version", http.StatusBadRequest)
		return
	}
	call := agents.Call{Authority: authority, Publication: revision, Revisions: snapshot.Snapshot.AgentPublications, ReleaseID: releaseAffinity(snapshot), Version: version, Revalidate: func(ctx context.Context, pinned agentcontract.Revision) bool {
		return s.streamAuthorized(r.WithContext(ctx), authority, pinned)
	}}
	switch value := decoded.(type) {
	case sendCommand:
		if value.streaming {
			s.stream(w, request.ID, version, s.executor.SendStream(r.Context(), call, value.params))
			return
		}
		result, err := s.executor.Send(r.Context(), call, value.params)
		if err != nil {
			reply(w, request.ID, nil, rpcFailure(err))
			return
		}
		data, marshalErr := marshalEvent(version, result)
		if marshalErr != nil {
			reply(w, request.ID, nil, rpcFailure(agents.ErrInternal))
			return
		}
		reply(w, request.ID, data, nil)
	case getCommand:
		result, err := s.executor.Get(r.Context(), call, value.params)
		replyTask(w, request.ID, version, result, err)
	case cancelCommand:
		result, err := s.executor.Cancel(r.Context(), call, value.params)
		replyTask(w, request.ID, version, result, err)
	case subscribeCommand:
		s.stream(w, request.ID, version, s.executor.Subscribe(r.Context(), call, value.params))
	}
}
func releaseAffinity(snapshot *release.Release) string {
	if id, err := uuid.Parse(snapshot.ID); err == nil && id != uuid.Nil && id.String() == snapshot.ID {
		return snapshot.ID
	}
	return snapshot.Snapshot.Generation.ID
}
func replyTask(w http.ResponseWriter, id json.RawMessage, version string, task *protocol.Task, err error) {
	if err != nil {
		reply(w, id, nil, rpcFailure(err))
		return
	}
	data, marshalErr := marshalTask(version, task)
	if marshalErr != nil {
		reply(w, id, nil, rpcFailure(agents.ErrInternal))
		return
	}
	reply(w, id, data, nil)
}
func rpcFailure(err error) *rpcError {
	switch {
	case errors.Is(err, agents.ErrInvalidParameters):
		return &rpcError{-32602, err.Error()}
	case errors.Is(err, agents.ErrTaskNotFound):
		return &rpcError{-32001, err.Error()}
	case errors.Is(err, agents.ErrUnboundedWork):
		return &rpcError{-32002, err.Error()}
	case errors.Is(err, tasks.ErrTaskLimit):
		return &rpcError{-32002, "Agent task limit reached"}
	case errors.Is(err, tasks.ErrRateLimit):
		return &rpcError{-32002, "Agent send rate exceeded"}
	}
	var failure agents.Failure
	if errors.As(err, &failure) {
		return &rpcError{-32603, failure.Error()}
	}
	return &rpcError{-32603, "Internal error"}
}
func (s *Server) stream(w http.ResponseWriter, id json.RawMessage, version string, events iter.Seq2[protocol.Event, error]) {
	if _, ok := w.(http.Flusher); !ok {
		reply(w, id, nil, rpcFailure(agents.ErrStreaming))
		return
	}
	next, stop := iter.Pull2(events)
	defer stop()
	first, err, ok := next()
	if err != nil {
		reply(w, id, nil, rpcFailure(err))
		return
	}
	if !ok {
		reply(w, id, nil, rpcFailure(agents.ErrInternal))
		return
	}
	streamHeaders(w)
	if !writeStreamEvent(w, id, first, version) {
		return
	}
	for {
		event, eventErr, more := next()
		if !more {
			return
		}
		if eventErr != nil {
			writeStreamFailure(w, id, rpcFailure(eventErr).Message)
			return
		}
		if !writeStreamEvent(w, id, event, version) {
			return
		}
	}
}

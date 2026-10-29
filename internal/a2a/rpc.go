package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/runtime"
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
	authority, revision, release, status := s.resolve(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	version := requestProtocol(r)
	if version == "" {
		http.Error(w, "Unsupported A2A version", http.StatusBadRequest)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Unsupported binding", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRPCBytes+1))
	if err != nil || len(body) > maxRPCBytes {
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
	decoded, err := decodeRequest(version, request)
	if err != nil {
		if version != revision.Definition.Agent.Protocol {
			http.Error(w, "Unsupported A2A version", http.StatusBadRequest)
			return
		}
		if errors.Is(err, errMethodNotDeclared) {
			fail(w, request.ID, -32601, "Method not found")
			return
		}
		fail(w, request.ID, -32602, "Invalid parameters")
		return
	}
	if version != revision.Definition.Agent.Protocol && (decoded.Method == "SendMessage" || decoded.Method == "SendStreamingMessage") {
		var params protocol.SendMessageRequest
		if json.Unmarshal(decoded.Params, &params) != nil || params.Message == nil || params.Message.TaskID == "" {
			http.Error(w, "Unsupported A2A version", http.StatusBadRequest)
			return
		}
	}
	switch decoded.Method {
	case "SendMessage":
		s.send(w, r, authority, revision, release, version, decoded)
	case "GetTask":
		s.getTask(w, r, authority, revision, release, version, decoded)
	case "SendStreamingMessage":
		s.sendStream(w, r, authority, revision, release, version, decoded)
	case "SubscribeToTask":
		s.subscribe(w, r, authority, revision, release, version, decoded)
	default:
		fail(w, request.ID, -32601, "Method not found")
	}
}

func (s *Server) pinnedRevision(authority access.Authority, publication agentcontract.Revision, task task, release *runtime.Release, version string) (agentcontract.Revision, bool) {
	if release == nil || release.Snapshot == nil {
		return agentcontract.Revision{}, false
	}
	for _, revision := range release.Snapshot.AgentPublications {
		if revision.ID == task.RevisionID && revision.PublicationID == publication.PublicationID && revision.ProjectID == publication.ProjectID && revision.Definition.Agent != nil && revision.Definition.Agent.Protocol == version && s.Runtime.AuthorizeAgentResource(authority, "agents", revision.PublicationID, revision.ID, revision.ProjectID) == nil {
			return revision, true
		}
	}
	return agentcontract.Revision{}, false
}

type preparedSend struct {
	Params             protocol.SendMessageRequest
	Revision           agentcontract.Revision
	ExpectedUpstreamID string
}

func (s *Server) prepareSend(ctx context.Context, authority access.Authority, revision agentcontract.Revision, release *runtime.Release, version string, raw json.RawMessage) (preparedSend, *rpcError) {
	var params protocol.SendMessageRequest
	if json.Unmarshal(raw, &params) != nil || params.Message == nil || params.Message.ID == "" || params.Message.Role != protocol.MessageRoleUser || len(params.Message.Parts) == 0 || len(params.Message.ReferenceTasks) != 0 || params.Tenant != "" || params.Config != nil && params.Config.PushConfig != nil {
		return preparedSend{}, &rpcError{-32602, "Invalid parameters"}
	}
	if params.Message.TaskID == "" {
		if params.Message.ContextID != "" {
			return preparedSend{}, &rpcError{-32602, "Invalid parameters"}
		}
		return preparedSend{Params: params, Revision: revision}, nil
	}
	owned, err := s.Ledger.findTask(ctx, authority, revision.PublicationID, string(params.Message.TaskID))
	if err != nil {
		return preparedSend{}, &rpcError{-32001, "Task not found"}
	}
	pinned, ok := s.pinnedRevision(authority, revision, owned, release, version)
	if !ok || params.Message.ContextID != "" && params.Message.ContextID != owned.ID {
		return preparedSend{}, &rpcError{-32001, "Task not found"}
	}
	translatedMessage := protocol.Message{ID: params.Message.ID, ContextID: owned.ContextID, Extensions: params.Message.Extensions, Metadata: params.Message.Metadata, Parts: params.Message.Parts, Role: params.Message.Role, TaskID: protocol.TaskID(owned.UpstreamID)}
	return preparedSend{Params: protocol.SendMessageRequest{Config: params.Config, Message: &translatedMessage, Metadata: params.Metadata}, Revision: pinned, ExpectedUpstreamID: owned.UpstreamID}, nil
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, authority access.Authority, revision agentcontract.Revision, release *runtime.Release, version string, request rpcRequest) {
	prepared, failure := s.prepareSend(r.Context(), authority, revision, release, version, request.Params)
	if failure != nil {
		reply(w, request.ID, nil, failure)
		return
	}
	if _, err := s.loadCard(r.Context(), *prepared.Revision.Definition.Agent); err != nil {
		fail(w, request.ID, -32603, "Internal error")
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
	if err := s.Runtime.AuthorizeAgentResource(authority, "agents", prepared.Revision.PublicationID, prepared.Revision.ID, prepared.Revision.ProjectID); err != nil {
		s.Ledger.release(r.Context(), res)
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	if err := s.Ledger.Store.MarkDispatched(r.Context(), res.DispatchID, res.ClaimID); err != nil {
		s.Ledger.release(r.Context(), res)
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	upstream, err := transport.SendMessage(r.Context(), headers, &prepared.Params)
	if err != nil {
		s.Ledger.unknown(r.Context(), res)
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	upstreamID, upstreamContext, ok := resultIDs(upstream)
	if !ok || prepared.ExpectedUpstreamID != "" && upstreamID != prepared.ExpectedUpstreamID {
		s.Ledger.unknown(r.Context(), res)
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	publicID, err := s.Ledger.settle(settleCtx, res, authority, prepared.Revision, upstreamID, upstreamContext)
	if err != nil {
		s.Ledger.unknown(r.Context(), res)
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	result, err := marshalEvent(prepared.Revision.Definition.Agent.Protocol, publicResult(upstream, publicID))
	if err != nil {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	reply(w, request.ID, result, nil)
}

func resultIDs(result protocol.SendMessageResult) (string, string, bool) {
	switch v := result.(type) {
	case *protocol.Task:
		if v == nil || v.ID == "" || v.ContextID == "" {
			return "", "", false
		}
		return string(v.ID), v.ContextID, true
	case *protocol.Message:
		if v == nil || v.ID == "" {
			return "", "", false
		}
		return string(v.TaskID), v.ContextID, true
	default:
		return "", "", false
	}
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, authority access.Authority, revision agentcontract.Revision, release *runtime.Release, version string, request rpcRequest) {
	var params protocol.GetTaskRequest
	if json.Unmarshal(request.Params, &params) != nil || params.ID == "" || params.Tenant != "" {
		fail(w, request.ID, -32602, "Invalid parameters")
		return
	}
	owned, err := s.Ledger.findTask(r.Context(), authority, revision.PublicationID, string(params.ID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, request.ID, -32001, "Task not found")
		} else {
			fail(w, request.ID, -32603, "Internal error")
		}
		return
	}
	pinned, ok := s.pinnedRevision(authority, revision, owned, release, version)
	if !ok {
		fail(w, request.ID, -32001, "Task not found")
		return
	}
	if _, err := s.loadCard(r.Context(), *pinned.Definition.Agent); err != nil {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	if err := s.Runtime.AuthorizeAgentResource(authority, "agents", pinned.PublicationID, pinned.ID, pinned.ProjectID); err != nil {
		fail(w, request.ID, -32001, "Task not found")
		return
	}
	transport, headers, cleanup, err := s.transport(r.Context(), *pinned.Definition.Agent)
	if err != nil {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	defer cleanup()
	upstream, err := transport.GetTask(r.Context(), headers, &protocol.GetTaskRequest{ID: protocol.TaskID(owned.UpstreamID), HistoryLength: params.HistoryLength})
	if err != nil || upstream == nil || string(upstream.ID) != owned.UpstreamID {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	result, err := marshalTask(pinned.Definition.Agent.Protocol, publicTask(upstream, owned.ID))
	if err != nil {
		fail(w, request.ID, -32603, "Internal error")
		return
	}
	reply(w, request.ID, result, nil)
}

func publicResult(result protocol.SendMessageResult, id string) protocol.Event {
	switch v := result.(type) {
	case *protocol.Task:
		return publicTask(v, id)
	case *protocol.Message:
		return publicMessage(v, id)
	default:
		return nil
	}
}

func publicMessage(message *protocol.Message, id string) *protocol.Message {
	if message == nil {
		return nil
	}
	return &protocol.Message{ID: message.ID, ContextID: publicReference(message.ContextID, id), Extensions: message.Extensions, Metadata: message.Metadata, Parts: message.Parts, Role: message.Role, TaskID: protocol.TaskID(publicReference(string(message.TaskID), id))}
}

func publicReference(original, id string) string {
	if original == "" {
		return ""
	}
	return id
}

func publicTask(task *protocol.Task, id string) *protocol.Task {
	history := slices.Collect(func(yield func(*protocol.Message) bool) {
		for _, message := range task.History {
			if !yield(publicMessage(message, id)) {
				return
			}
		}
	})
	return &protocol.Task{ID: protocol.TaskID(id), ContextID: id, Artifacts: task.Artifacts, History: history, Metadata: task.Metadata, Status: protocol.TaskStatus{State: task.Status.State, Message: publicMessage(task.Status.Message, id), Timestamp: task.Status.Timestamp}}
}

type limitedTransport struct{ Base http.RoundTripper }

func (t limitedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.Base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		return limitedResponse(response, &boundedEventBody{ReadCloser: response.Body}, response.ContentLength), nil
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRPCBytes+1))
	if err != nil || len(data) > maxRPCBytes {
		return nil, errors.New("upstream response size")
	}
	return limitedResponse(response, io.NopCloser(bytes.NewReader(data)), int64(len(data))), nil
}

func limitedResponse(response *http.Response, body io.ReadCloser, length int64) *http.Response {
	return &http.Response{Status: response.Status, StatusCode: response.StatusCode, Proto: response.Proto, ProtoMajor: response.ProtoMajor, ProtoMinor: response.ProtoMinor, Header: response.Header, Body: body, ContentLength: length, TransferEncoding: response.TransferEncoding, Close: response.Close, Uncompressed: response.Uncompressed, Trailer: response.Trailer, Request: response.Request, TLS: response.TLS}
}

func (s *Server) transport(ctx context.Context, definition agentcontract.Definition) (a2aclient.Transport, a2aclient.ServiceParams, func(), error) {
	if s.Egress == nil {
		return nil, nil, nil, errors.New("egress unavailable")
	}
	if _, err := s.Egress.ValidateEndpoint(definition.Endpoint); err != nil {
		return nil, nil, nil, err
	}
	base := s.Egress.Client(30 * time.Second)
	client := &http.Client{Transport: limitedTransport{Base: base.Transport}, CheckRedirect: base.CheckRedirect, Jar: base.Jar, Timeout: base.Timeout}
	transport := upstreamTransport(definition.Protocol, definition.Endpoint, client)
	if definition.CredentialID == nil {
		return transport, a2aclient.ServiceParams{"A2A-Version": []string{string(cardVersion(definition.Protocol))}}, func() { _ = transport.Destroy() }, nil
	}
	if s.Credentials == nil {
		return nil, nil, nil, errors.New("credential unavailable")
	}
	credential, err := s.Credentials(ctx, *definition.CredentialID)
	if err != nil || len(credential) == 0 {
		return nil, nil, nil, errors.New("credential unavailable")
	}
	return transport, a2aclient.ServiceParams{"Authorization": []string{"Bearer " + string(credential)}, "A2A-Version": []string{string(cardVersion(definition.Protocol))}}, func() { clear(credential); _ = transport.Destroy() }, nil
}

func upstreamTransport(version, endpoint string, client *http.Client) a2aclient.Transport {
	if version == legacyProtocol {
		return a2av0.NewJSONRPCTransport(a2av0.JSONRPCTransportConfig{URL: endpoint, Client: client})
	}
	return a2aclient.NewJSONRPCTransport(endpoint, client)
}

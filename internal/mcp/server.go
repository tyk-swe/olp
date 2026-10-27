package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Status string

const (
	StatusSucceeded      Status = "succeeded"
	StatusUpstreamError  Status = "upstream_error"
	StatusOutcomeUnknown Status = "outcome_unknown"

	defaultCallTimeout = 30 * time.Second
	maxCallBodyBytes   = 1 << 20
)

type Runtime interface {
	Authenticate(secret string) (*access.Authority, error)
	Release() *runtime.Release
	AuthorizeAgentResource(a access.Authority, scope, id, revision, project string) error
}

type ReserveInput struct {
	Authority         access.Authority
	ToolsetID         string
	ToolsetRevisionID string
	ReleaseID         string
	ToolAlias         string
	InputDigest       string
}

type Reservation struct {
	DispatchID        string
	ClaimID           string
	RunID             string
	StepID            string
	AttemptID         string
	APIKeyID          string
	ProjectID         string
	ToolsetID         string
	ToolsetRevisionID string
	ToolAlias         string
	InputDigest       string
}

type AttemptEvidence struct {
	ServerID         string
	ServerRevisionID string
	Status           Status
	IsError          bool
	ResultDigest     string
	StartedAt        time.Time
	CompletedAt      time.Time
}

type Ledger interface {
	Reserve(ctx context.Context, in ReserveInput) (*Reservation, error)
	MarkDispatched(ctx context.Context, reservation *Reservation) error
	ReleaseUndispatched(ctx context.Context, reservation *Reservation) error
	MarkUnknown(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error
	Settle(ctx context.Context, reservation *Reservation, evidence AttemptEvidence) error
}

type Server struct {
	Runtime        Runtime
	Egress         *egress.Policy
	Ledger         Ledger
	Auth           *secrets.AuthKey
	Sessions       *SessionStore
	TrustedProxies []netip.Prefix
	Credentials    func(ctx context.Context, credentialID string) ([]byte, error)
	Now            func() time.Time
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/mcp/tools", s.listTools)
	mux.HandleFunc("POST /v1/mcp/tools/call", s.callTool)
	for _, path := range []string{"/mcp", "/mcp/"} {
		mux.HandleFunc("POST "+path, s.serveStreamableHTTP)
		mux.HandleFunc("GET "+path, s.serveStreamableGET)
		mux.HandleFunc("DELETE "+path, s.serveStreamableDELETE)
	}
}

func releaseIDFor(release *runtime.Release, snapshot *routing.Snapshot) string {
	if canonicalUUID(release.ID) {
		return release.ID
	}
	return snapshot.Generation.ID
}

type apiError struct {
	status int
	code   string
	kind   string
	msg    string
}

func fail(status int, code, kind, msg string) *apiError {
	return &apiError{status: status, code: code, kind: kind, msg: msg}
}

var (
	errToolsetNotFound          = fail(http.StatusNotFound, "toolset_not_found", "invalid_request_error", "The toolset does not exist or this key may not use it.")
	errToolNotFound             = fail(http.StatusNotFound, "tool_not_found", "invalid_request_error", "The tool does not exist in the pinned toolset.")
	errLedger                   = fail(http.StatusServiceUnavailable, "debit_ledger_unavailable", "api_error", "The durable debit ledger is unavailable, so the call was not dispatched.")
	errSessionRequired          = fail(http.StatusBadRequest, "session_required", "invalid_request_error", "Supply the session in the Mcp-Session-Id header.")
	errSessionNotFound          = fail(http.StatusNotFound, "session_not_found", "invalid_request_error", "The session does not exist.")
	errSessionProtocolMismatch  = fail(http.StatusBadRequest, "protocol_version_mismatch", "invalid_request_error", "The protocol version does not match the session.")
	errSessionUnavailable       = fail(http.StatusServiceUnavailable, "session_unavailable", "api_error", "The session store is unavailable; retry shortly.")
	errSessionCreateUnavailable = fail(http.StatusServiceUnavailable, "session_unavailable", "api_error", "A session could not be created, so nothing was dispatched.")
	errSessionNotInitialized    = fail(http.StatusBadRequest, "session_not_initialized", "invalid_request_error", "Send notifications/initialized before using this session.")
	errSessionLimit             = fail(http.StatusTooManyRequests, "session_limit_exceeded", "rate_limit_error", "This key already has the maximum number of active sessions.")
)

func writeError(w http.ResponseWriter, err *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": err.msg, "type": err.kind, "code": err.code}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOutcomeUnknown(w http.ResponseWriter, reservation *Reservation, message string) {
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"attempt_id": reservation.AttemptID, "dispatch_id": reservation.DispatchID, "status": StatusOutcomeUnknown,
		"error": map[string]any{"message": message, "type": "api_error", "code": "outcome_unknown"},
	})
}

func canonicalUUID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}

func (s *Server) authenticate(r *http.Request) (access.Authority, *apiError) {
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	authority, failure := consumerauth.Authenticate(s.Runtime, r, token, "tools", s.TrustedProxies, s.now())
	if failure != nil {
		kind := "api_error"
		switch failure.Status {
		case http.StatusUnauthorized:
			kind = "authentication_error"
		case http.StatusForbidden:
			kind = "permission_error"
		}
		return access.Authority{}, fail(failure.Status, failure.Code, kind, failure.Message)
	}
	if authority.ProjectID == nil {
		return access.Authority{}, fail(http.StatusForbidden, "permission_denied", "permission_error", "This API key does not have the tools scope for a project.")
	}
	return authority, nil
}

type resolvedToolset struct {
	id         string
	revisionID string
	tools      []routing.MCPToolsetTool
}

func (s *Server) snapshot() (*runtime.Release, *routing.Snapshot, *apiError) {
	if s.Runtime == nil {
		return nil, nil, fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Tool serving is unavailable; retry shortly.")
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		return nil, nil, fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Tool serving is unavailable; retry shortly.")
	}
	return release, release.Snapshot, nil
}

func currentToolsetRevision(snapshot *routing.Snapshot, toolsetID, project string) (string, bool) {
	for _, revision := range snapshot.AgentPublications {
		if revision.PublicationID == toolsetID && revision.Kind == "toolset" && revision.ProjectID == project && revision.Current {
			return revision.ID, true
		}
	}
	return "", false
}

func (s *Server) resolveToolset(authority access.Authority, snapshot *routing.Snapshot, toolsetID string) (resolvedToolset, *apiError) {
	if !canonicalUUID(toolsetID) {
		return resolvedToolset{}, errToolsetNotFound
	}
	revisionID, ok := currentToolsetRevision(snapshot, toolsetID, *authority.ProjectID)
	if !ok {
		return resolvedToolset{}, errToolsetNotFound
	}
	if err := s.Runtime.AuthorizeAgentResource(authority, "tools", toolsetID, revisionID, *authority.ProjectID); err != nil {
		if errors.Is(err, runtime.ErrStaleAuthority) {
			return resolvedToolset{}, fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly.")
		}
		return resolvedToolset{}, errToolsetNotFound
	}
	tools, ok := snapshot.MCPTools(revisionID)
	if !ok {
		return resolvedToolset{}, errToolsetNotFound
	}
	return resolvedToolset{id: toolsetID, revisionID: revisionID, tools: tools}, nil
}

type listedTool struct {
	ToolsetID         string          `json:"toolset_id"`
	ToolsetRevisionID string          `json:"toolset_revision_id"`
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	InputSchema       json.RawMessage `json:"input_schema"`
}

func (s *Server) listTools(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	_, snapshot, failure := s.snapshot()
	if failure != nil {
		writeError(w, failure)
		return
	}
	tools := []listedTool{}
	appendToolset := func(resolved resolvedToolset) {
		for _, tool := range resolved.tools {
			tools = append(tools, listedTool{ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID, Name: tool.Alias, Description: tool.Description, InputSchema: tool.InputSchema})
		}
	}
	if requested := r.URL.Query().Get("toolset_id"); requested != "" {
		resolved, failure := s.resolveToolset(authority, snapshot, requested)
		if failure != nil {
			writeError(w, failure)
			return
		}
		appendToolset(resolved)
	} else {
		for _, toolsetID := range authority.Policy.AllowedToolsets {
			resolved, failure := s.resolveToolset(authority, snapshot, toolsetID)
			if failure == nil {
				appendToolset(resolved)
			} else if failure.status == http.StatusServiceUnavailable {
				writeError(w, failure)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

type callRequest struct {
	ToolsetID string          `json:"toolset_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) toolInputDigest(arguments json.RawMessage) string {
	return hex.EncodeToString(s.Auth.Digest(secrets.ToolInputDigest, string(arguments)))
}

func (s *Server) release(reservation *Reservation) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Ledger.ReleaseUndispatched(ctx, reservation)
}

func (s *Server) callTool(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	release, snapshot, failure := s.snapshot()
	if failure != nil {
		writeError(w, failure)
		return
	}
	var request callRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCallBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, fail(http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error", "The call body exceeds the 1 MiB bound."))
			return
		}
		writeError(w, fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "Supply a tool call JSON object."))
		return
	}
	resolved, failure := s.resolveToolset(authority, snapshot, request.ToolsetID)
	if failure != nil {
		writeError(w, failure)
		return
	}
	index := slices.IndexFunc(resolved.tools, func(tool routing.MCPToolsetTool) bool { return tool.Alias == request.Name })
	if index < 0 {
		writeError(w, errToolNotFound)
		return
	}
	tool := resolved.tools[index]
	arguments := request.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	if err := tool.ValidateArguments(arguments); err != nil {
		writeError(w, fail(http.StatusUnprocessableEntity, "invalid_tool_arguments", "invalid_request_error", "The arguments do not match the pinned tool schema."))
		return
	}
	if s.Ledger == nil || s.Egress == nil || s.Auth == nil {
		writeError(w, errLedger)
		return
	}
	releaseID := releaseIDFor(release, snapshot)
	outcome := s.execute(r.Context(), executeInput{authority: authority, resolved: resolved, tool: tool, arguments: arguments, releaseID: releaseID})
	switch {
	case outcome.failure != nil:
		if outcome.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64((outcome.retryAfter+time.Second-1)/time.Second), 10))
		}
		writeError(w, outcome.failure)
	case outcome.outcomeUnknown:
		writeOutcomeUnknown(w, outcome.reservation, outcome.unknownMessage)
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"toolset_id": resolved.id, "toolset_revision_id": resolved.revisionID, "name": tool.Alias,
			"content": outcome.result.Content, "structured_content": outcome.result.StructuredContent, "is_error": outcome.result.IsError,
			"attempt_id": outcome.reservation.AttemptID, "dispatch_id": outcome.reservation.DispatchID,
		})
	}
}

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
	"sync"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/workload"
)

const (
	defaultCallTimeout = 30 * time.Second
	maxCallBodyBytes   = 1 << 20
)

type RateLimiter interface {
	Reserve(ctx context.Context, request admission.Request) (*admission.Lease, error)
}

func cloneUnitPrice(price *string) *string {
	if price == nil {
		return nil
	}
	out := *price
	return &out
}

type Runtime interface {
	consumerauth.TaskCapabilityAuthenticator
	ReauthenticateDelegation(context.Context, string, string) (*keyauthority.Authority, error)
	Authenticate(secret string) (*keyauthority.Authority, error)
	Release() *release.Release
	AuthorizeAgentResource(a keyauthority.Authority, scope, id, revision, project string) error
	AuthorizeMCPTool(a keyauthority.Authority, id, revision, project, server, serverRevision string) error
}

type GuardrailEvaluation struct {
	Authority keyauthority.Authority
	Release   *release.Release
	Engine    *guardrails.Engine
	RequestID string
	Phase     guardrails.Phase
	Segments  []guardrails.Segment
	Masks     *guardrails.Masking
}

type ServerOptions struct {
	Runtime        Runtime
	Executor       *Executor
	Auth           *secrets.AuthKey
	Grants         *personal.Store
	Sessions       *sessions.Store
	TrustedProxies []netip.Prefix
}

type Server struct {
	runtime        Runtime
	executor       *Executor
	auth           *secrets.AuthKey
	grants         *personal.Store
	sessions       *sessions.Store
	trustedProxies []netip.Prefix
	legacyStreams  sync.Map
}

func New(options ServerOptions) *Server {
	return &Server{runtime: options.Runtime, executor: options.Executor, auth: options.Auth,
		grants: options.Grants, sessions: options.Sessions, trustedProxies: slices.Clone(options.TrustedProxies)}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/mcp/servers/{server_id}/grant-enrollments", s.withAssertion(s.startGrantEnrollment))
	mux.HandleFunc("GET /v1/mcp/tools", s.withAssertion(s.listTools))
	mux.HandleFunc("POST /v1/mcp/tools/simulate", s.withAssertion(s.simulateTool))
	mux.HandleFunc("POST /v1/mcp/tools/call", s.withAssertion(s.callTool))
	mux.HandleFunc("GET /mcp/sse", s.withAssertion(s.serveLegacyGET))
	mux.HandleFunc("POST /mcp/messages", s.withAssertion(s.serveLegacyPOST))
	for _, path := range []string{"/mcp", "/mcp/"} {
		mux.HandleFunc("POST "+path, s.withAssertion(s.serveStreamableHTTP))
		mux.HandleFunc("GET "+path, s.withAssertion(s.serveStreamableGET))
		mux.HandleFunc("DELETE "+path, s.withAssertion(s.serveStreamableDELETE))
	}
}

func releaseIDFor(release *release.Release, snapshot *routing.View) string {
	if canonicalUUID(release.ID) {
		return release.ID
	}
	return snapshot.Generation().ID
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
	errToolDefinitionChanged    = fail(http.StatusConflict, "tool_definition_changed", "api_error", "The upstream tool definitions differ from the pinned published definitions, so the call was not dispatched. Publish a reviewed revision to use the changed definitions.")
	errToolSchemaInvalid        = fail(http.StatusServiceUnavailable, "tool_schema_invalid", "api_error", "The pinned tool schema could not be compiled, so the call was not dispatched.")
	errCatalogUnavailable       = fail(http.StatusBadGateway, "upstream_unavailable", "api_error", "The upstream MCP server catalog could not be verified, so the session was not opened.")
	errPermissionDenied         = fail(http.StatusForbidden, "permission_denied", "permission_error", "This API key may no longer use the toolset.")
	errLedger                   = fail(http.StatusServiceUnavailable, "debit_ledger_unavailable", "api_error", "The durable debit ledger is unavailable, so the call was not dispatched.")
	errSessionRequired          = fail(http.StatusBadRequest, "session_required", "invalid_request_error", "Supply the session in the Mcp-Session-Id header.")
	errSessionNotFound          = fail(http.StatusNotFound, "session_not_found", "invalid_request_error", "The session does not exist.")
	errSessionProtocolMismatch  = fail(http.StatusBadRequest, "protocol_version_mismatch", "invalid_request_error", "The protocol version does not match the session.")
	errSessionUnavailable       = fail(http.StatusServiceUnavailable, "session_unavailable", "api_error", "The session store is unavailable; retry shortly.")
	errSessionCreateUnavailable = fail(http.StatusServiceUnavailable, "session_unavailable", "api_error", "A session could not be created, so nothing was dispatched.")
	errSessionNotInitialized    = fail(http.StatusBadRequest, "session_not_initialized", "invalid_request_error", "Send notifications/initialized before using this session.")
	errSessionLimit             = fail(http.StatusTooManyRequests, "session_limit_exceeded", "rate_limit_error", "This key already has the maximum number of active sessions.")
	errCallIDRequired           = fail(http.StatusBadRequest, "call_id_required", "invalid_request_error", "Supply a caller call identifier for this tool call.")
	errCallIDInvalid            = fail(http.StatusBadRequest, "invalid_call_id", "invalid_request_error", "Supply a caller call identifier of 1-256 bytes without control characters.")
)

func callIDFailure(callID *string) *apiError {
	if callID == nil {
		return errCallIDRequired
	}
	if !validCallID(*callID) {
		return errCallIDInvalid
	}
	return nil
}

func canonicalUUID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}

type delegatedSessionAuthenticator struct{ Runtime }

func (a delegatedSessionAuthenticator) AuthenticateTaskCapability(ctx context.Context, token string) (*keyauthority.Authority, error) {
	return a.Runtime.AuthenticateTaskCapability(ctx, token)
}

func (a delegatedSessionAuthenticator) AuthenticateDelegation(ctx context.Context, token, assertion string) (*keyauthority.Authority, error) {
	return a.Runtime.ReauthenticateDelegation(ctx, token, assertion)
}

func (s *Server) authenticate(r *http.Request) (keyauthority.Authority, *apiError) {
	return s.authenticateWith(r, s.runtime)
}

func (s *Server) authenticateSession(r *http.Request) (keyauthority.Authority, *apiError) {
	if s.runtime == nil || (r.Header.Get("Mcp-Session-Id") == "" && r.URL.Query().Get("session_id") == "") {
		return s.authenticate(r)
	}
	return s.reauthenticateSession(r)
}

func (s *Server) reauthenticateSession(r *http.Request) (keyauthority.Authority, *apiError) {
	if s.runtime == nil {
		return s.authenticate(r)
	}
	return s.authenticateWith(r, delegatedSessionAuthenticator{s.runtime})
}

func (s *Server) authenticateWith(r *http.Request, auth consumerauth.Authenticator) (keyauthority.Authority, *apiError) {
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	authority, failure := consumerauth.Authenticate(auth, r, token, "tools", s.trustedProxies, time.Now())
	if failure != nil {
		kind := "api_error"
		switch failure.Status {
		case http.StatusUnauthorized:
			kind = "authentication_error"
		case http.StatusForbidden:
			kind = "permission_error"
		}
		return keyauthority.Authority{}, fail(failure.Status, failure.Code, kind, failure.Message)
	}
	if authority.ProjectID == nil {
		return keyauthority.Authority{}, fail(http.StatusForbidden, "permission_denied", "permission_error", "This API key does not have the tools scope for a project.")
	}
	return s.identifyEndUser(r, authority)
}

func (s *Server) identifyEndUser(r *http.Request, authority keyauthority.Authority) (keyauthority.Authority, *apiError) {
	if authority.TaskCapability != nil || authority.WorkloadIssuerID != nil && authority.EndUserDigest != "" {
		return authority, nil
	}
	if authority.Policy.EndUserSource == nil {
		if authority.ProjectEndUserPolicy != nil || authority.Policy.EndUserPolicy != nil {
			return keyauthority.Authority{}, fail(http.StatusBadRequest, "end_user_source_required", "invalid_request_error", "This key must configure an end-user identifier source before using an end-user policy.")
		}
		return authority, nil
	}
	digest, failure := consumerauth.HeaderEndUserDigest(s.runtime, r, authority)
	if failure != nil {
		if failure.Status == http.StatusBadRequest {
			return keyauthority.Authority{}, fail(failure.Status, failure.Code, "invalid_request_error", failure.Message)
		}
		return keyauthority.Authority{}, fail(failure.Status, failure.Code, "api_error", failure.Message)
	}
	authority.EndUserDigest = digest
	return authority, nil
}

type resolvedToolset struct {
	id         string
	revisionID string
	tools      []routing.MCPToolsetTool
	drifted    []routing.MCPDriftedTool
	invalid    []routing.MCPDriftedTool
	missing    []routing.MCPDriftedTool
}

func toolsetFromResolution(id, revisionID string, resolution routing.MCPToolsetResolution) resolvedToolset {
	return resolvedToolset{
		id: id, revisionID: revisionID, tools: resolution.Tools,
		drifted: resolution.Drifted, invalid: resolution.Invalid, missing: resolution.Missing,
	}
}

type toolLookup int

const (
	toolFound toolLookup = iota
	toolDrifted
	toolInvalidSchema
	toolServerMissing
	toolMissing
)

func toolLookupFailure(found toolLookup) *apiError {
	switch found {
	case toolFound:
		return nil
	case toolDrifted:
		return errToolDefinitionChanged
	case toolInvalidSchema:
		return errToolSchemaInvalid
	default:
		return errToolNotFound
	}
}

func (r resolvedToolset) lookup(alias string) (routing.MCPToolsetTool, toolLookup) {
	if index := slices.IndexFunc(r.tools, func(tool routing.MCPToolsetTool) bool { return tool.Alias == alias }); index >= 0 {
		return r.tools[index], toolFound
	}
	if slices.ContainsFunc(r.drifted, func(pin routing.MCPDriftedTool) bool { return pin.Alias == alias }) {
		return routing.MCPToolsetTool{}, toolDrifted
	}
	if slices.ContainsFunc(r.invalid, func(pin routing.MCPDriftedTool) bool { return pin.Alias == alias }) {
		return routing.MCPToolsetTool{}, toolInvalidSchema
	}
	if slices.ContainsFunc(r.missing, func(pin routing.MCPDriftedTool) bool { return pin.Alias == alias }) {
		return routing.MCPToolsetTool{}, toolServerMissing
	}
	return routing.MCPToolsetTool{}, toolMissing
}

func (r resolvedToolset) listingFailure() *apiError {
	if len(r.drifted) > 0 {
		return errToolDefinitionChanged
	}
	if len(r.invalid) > 0 {
		return errToolSchemaInvalid
	}
	return nil
}

func (s *Server) driftScoped(authority keyauthority.Authority, resolved resolvedToolset) bool {
	return len(resolved.drifted) > 0 && len(resolved.missing) == 0 &&
		s.runtime.AuthorizeMCPTool(authority, resolved.id, resolved.revisionID, *authority.ProjectID, "", "") == nil
}

func (s *Server) snapshot() (*release.Release, *routing.View, *apiError) {
	if s.runtime == nil {
		return nil, nil, fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Tool serving is unavailable; retry shortly.")
	}
	release := s.runtime.Release()
	if release == nil || release.Snapshot() == nil {
		return nil, nil, fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Tool serving is unavailable; retry shortly.")
	}
	return release, release.Snapshot(), nil
}

func currentToolsetRevision(snapshot *routing.View, toolsetID, project string) (string, bool) {
	for _, revision := range snapshot.AgentPublications() {
		if revision.PublicationID == toolsetID && revision.Kind == "toolset" && revision.ProjectID == project && revision.Current {
			return revision.ID, true
		}
	}
	return "", false
}

func (s *Server) resolveToolset(authority keyauthority.Authority, snapshot *routing.View, toolsetID string) (resolvedToolset, *apiError) {
	if !canonicalUUID(toolsetID) {
		return resolvedToolset{}, errToolsetNotFound
	}
	revisionID, ok := currentToolsetRevision(snapshot, toolsetID, *authority.ProjectID)
	if !ok {
		return resolvedToolset{}, errToolsetNotFound
	}
	if err := s.runtime.AuthorizeAgentResource(authority, "tools", toolsetID, revisionID, *authority.ProjectID); err != nil {
		if errors.Is(err, release.ErrStaleAuthority) {
			return resolvedToolset{}, fail(http.StatusServiceUnavailable, "authority_unavailable", "api_error", "Key authority is unavailable; retry shortly.")
		}
		resolution, ok := snapshot.MCPToolset(revisionID)
		if !ok {
			return resolvedToolset{}, errToolsetNotFound
		}
		resolved := toolsetFromResolution(toolsetID, revisionID, resolution)
		if !s.driftScoped(authority, resolved) {
			return resolvedToolset{}, errToolsetNotFound
		}
		if len(resolved.tools) == 0 {
			return resolvedToolset{}, errToolDefinitionChanged
		}
		return resolved, nil
	}
	resolution, ok := snapshot.MCPToolset(revisionID)
	if !ok {
		return resolvedToolset{}, errToolsetNotFound
	}
	return toolsetFromResolution(toolsetID, revisionID, resolution), nil
}

type listedTool struct {
	ToolsetID         string                  `json:"toolset_id"`
	ToolsetRevisionID string                  `json:"toolset_revision_id"`
	Name              string                  `json:"name"`
	Description       string                  `json:"description"`
	InputSchema       json.RawMessage         `json:"input_schema"`
	UnitPrice         *string                 `json:"unit_price"`
	PricingState      accounting.PricingState `json:"pricing_state"`
	Limits            toolCallLimits          `json:"limits"`
	BudgetRequired    bool                    `json:"budget_required"`
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
			tools = append(tools, describeTool(authority, resolved, tool))
		}
	}
	if requested := r.URL.Query().Get("toolset_id"); requested != "" {
		resolved, failure := s.resolveToolset(authority, snapshot, requested)
		if failure != nil {
			writeError(w, failure)
			return
		}
		if failure = resolved.listingFailure(); failure != nil {
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
	writeJSON(w, http.StatusOK, toolsResponse{Tools: tools})
}

type executionSelection struct {
	Transport        json.RawMessage `json:"transport"`
	Stdio            json.RawMessage `json:"stdio"`
	Executable       json.RawMessage `json:"executable"`
	Command          json.RawMessage `json:"command"`
	Path             json.RawMessage `json:"path"`
	Args             json.RawMessage `json:"args"`
	Env              json.RawMessage `json:"env"`
	Environment      json.RawMessage `json:"environment"`
	Cwd              json.RawMessage `json:"cwd"`
	WorkingDirectory json.RawMessage `json:"working_directory"`
}

func (s executionSelection) supplied() bool {
	for _, value := range [...]json.RawMessage{s.Transport, s.Stdio, s.Executable, s.Command, s.Path, s.Args, s.Env, s.Environment, s.Cwd, s.WorkingDirectory} {
		if len(value) > 0 {
			return true
		}
	}
	return false
}

type callRequest struct {
	executionSelection
	ToolsetID string          `json:"toolset_id"`
	Name      string          `json:"name"`
	CallID    *string         `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
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
	request, decodeFailure := decodeCall(w, r)
	if decodeFailure != nil {
		writeError(w, decodeFailure)
		return
	}
	if failure := callIDFailure(request.CallID); failure != nil {
		writeError(w, failure)
		return
	}
	prepared, preparationFailure := s.prepareCall(authority, snapshot, request)
	if preparationFailure != nil {
		writeError(w, preparationFailure)
		return
	}
	resolved, tool, arguments := prepared.resolved, prepared.tool, prepared.arguments
	if !s.executor.available() {
		writeError(w, errLedger)
		return
	}
	releaseID := releaseIDFor(release, snapshot)
	outcome := s.executor.Execute(r.Context(), CallRequest{Authority: authority, ToolsetID: resolved.id, ToolsetRevisionID: resolved.revisionID, Tool: tool, Arguments: arguments, ReleaseID: releaseID, CallID: *request.CallID, Release: release, Policies: snapshot})
	writeExecution(w, resolved, tool, outcome)
}

func (s *Server) withAssertion(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
		handler(w, r.WithContext(workload.WithAssertion(r.Context(), raw)))
	}
}

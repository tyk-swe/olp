package mcp

import (
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcpgrants"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) startGrantEnrollment(w http.ResponseWriter, r *http.Request) {
	authority, failure := s.authenticate(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if authority.WorkloadIssuerID == nil || authority.EndUserDigest == "" {
		writeError(w, fail(http.StatusForbidden, "verified_individual_required", "permission_error", "A verified workload identity with an end user is required."))
		return
	}
	serverID, err := access.IDParam(r, "server_id")
	if err != nil {
		writeError(w, fail(http.StatusBadRequest, "invalid_server_id", "invalid_request_error", "Use a server UUID."))
		return
	}
	var input struct {
		ToolsetID string `json:"toolset_id"`
	}
	if err := access.Decode(r, &input); err != nil {
		writeError(w, fail(http.StatusBadRequest, "invalid_request", "invalid_request_error", "Supply a toolset identifier."))
		return
	}
	_, snapshot, failure := s.snapshot()
	if failure != nil {
		writeError(w, failure)
		return
	}
	resolved, failure := s.resolveToolset(authority, snapshot, input.ToolsetID)
	if failure != nil {
		writeError(w, failure)
		return
	}
	index := slices.IndexFunc(resolved.tools, func(tool routing.MCPToolsetTool) bool { return tool.ServerID == serverID })
	if index < 0 {
		writeError(w, fail(http.StatusNotFound, "server_not_found", "invalid_request_error", "The server was not found in this toolset."))
		return
	}
	if s.Grants == nil {
		writeError(w, fail(http.StatusServiceUnavailable, "grant_store_unavailable", "server_error", "Grant enrollment is unavailable."))
		return
	}
	bound, err := mcpgrants.Bind(authority, resolved.tools[index])
	if err != nil {
		writeError(w, fail(http.StatusBadRequest, "oauth_not_configured", "invalid_request_error", "The server has no personal OAuth configuration."))
		return
	}
	handoff, err := s.Grants.Start(r.Context(), bound, nil)
	if err != nil {
		writeError(w, fail(http.StatusConflict, "grant_binding_changed", "invalid_request_error", "The server changed. Start enrollment again."))
		return
	}
	writeJSON(w, http.StatusCreated, handoff)
}

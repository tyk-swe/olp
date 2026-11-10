package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/client"
)

func (s *Server) ServeModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.Host.AdmitInFlight(r.Context()) {
		s.writeError(w, r, code.Refuse(503, "code_overloaded"))
		return
	}
	defer s.Host.ReleaseInFlight(r.Context())
	authority, status, ok := s.Host.AuthenticatePresentedKey(r)
	if !ok {
		s.writeError(w, r, code.Refuse(status, "code_authentication_refused"))
		return
	}
	rel := s.Host.Release()
	if rel == nil || rel.Snapshot() == nil {
		s.writeError(w, r, code.Refuse(503, "code_runtime_unavailable"))
		return
	}
	route, ok := rel.Snapshot().FindCodeRoute(r.PathValue("slug"))
	if !ok || !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, s.Host.Now()) {
		s.writeError(w, r, code.Refuse(404, "code_route_unavailable"))
		return
	}
	if s.Ledger == nil {
		s.writeError(w, r, code.Refuse(503, "code_admission_unavailable"))
		return
	}
	offers, err := s.Ledger.Models(r.Context(), route, authority.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Object string       `json:"object"`
		Data   []code.Model `json:"data"`
	}{Object: "list", Data: client.Catalog(route, offers)})
}

func RefusalFromError(err error) *code.Refusal {
	if refusal, ok := errors.AsType[*code.Refusal](err); ok {
		return refusal
	}
	return &code.Refusal{Status: 503, Code: "code_service_unavailable"}
}

func RefusalMessage(errCode string) string {
	switch errCode {
	case "code_recovery_unsafe", "code_account_unavailable":
		return "Continue on the original account when available, or start a new conversation with full context and no response references, reasoning signatures or socket state"
	case "code_parent_unresolved":
		return "Establish the parent conversation first, or start a new conversation with full context and no response reference"
	case "code_connection_model_required":
		return "Set X-OLP-Code-Model to a route model before connecting. If Retry-After is present, at least one model-hinted connection may be available after that delay"
	case "code_response_account_conflict":
		return "Continue on the account that produced the response, or start a new conversation with full context and no response reference"
	case "code_translation_unsupported_semantics":
		return "This route supports text, tools and explicit reasoning effort. Use a native Messages account for required semantics this route cannot represent."
	}
	return errCode
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	s.Host.WriteRefusal(w, r, RefusalFromError(err))
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, route code.Route, keyID, digest string, err error) {
	refusal := RefusalFromError(err)
	s.recordRefusal(r.Context(), route, keyID, digest, refusal.Code)
	s.Host.WriteRefusal(w, r, refusal)
}

func (s *Server) recordRefusal(ctx context.Context, route code.Route, keyID, digest, refusalCode string) {
	if s.Ledger == nil {
		return
	}
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = s.Ledger.RecordRefusal(step, route, keyID, digest, refusalCode)
}

package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
)

func (s *Server) codeModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.admit(r.Context()) {
		codeWriteError(w, r, codemode.Refuse(503, "code_overloaded"))
		return
	}
	defer s.release(r.Context())
	token, failure := presentedKey(r)
	if failure != nil {
		codeWriteError(w, r, codemode.Refuse(failure.Status, "code_authentication_refused"))
		return
	}
	authority, failure := s.authenticateKey(r, token, "inference")
	if failure != nil {
		codeWriteError(w, r, codemode.Refuse(failure.Status, "code_authentication_refused"))
		return
	}
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		codeWriteError(w, r, codemode.Refuse(503, "code_runtime_unavailable"))
		return
	}
	route, ok := release.Snapshot.CodeRoutes[r.PathValue("slug")]
	if !ok {
		codeWriteError(w, r, codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	if !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, s.now()) {
		codeWriteError(w, r, codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	if s.CodeLedger == nil {
		codeWriteError(w, r, codemode.Refuse(503, "code_admission_unavailable"))
		return
	}
	offers, err := s.CodeLedger.Models(r.Context(), route, authority.ID)
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	writeJSON(w, struct {
		Object string           `json:"object"`
		Data   []codemode.Model `json:"data"`
	}{Object: "list", Data: codeclient.Catalog(route, offers)})
}

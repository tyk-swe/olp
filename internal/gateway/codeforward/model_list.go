package codeforward

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
)

func (f *Forwarder) codeModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !f.gate.Admit(r.Context()) {
		codeWriteError(w, r, codemode.Refuse(503, "code_overloaded"))
		return
	}
	defer f.gate.Release(r.Context())
	token, failure := ingress.PresentedKey(r)
	if failure != nil {
		codeWriteError(w, r, codemode.Refuse(failure.Status, "code_authentication_refused"))
		return
	}
	authority, failure := f.gate.AuthenticateKey(r, token, "inference")
	if failure != nil {
		codeWriteError(w, r, codemode.Refuse(failure.Status, "code_authentication_refused"))
		return
	}
	release := f.runtime.Release()
	if release == nil || release.Snapshot() == nil {
		codeWriteError(w, r, codemode.Refuse(503, "code_runtime_unavailable"))
		return
	}
	route, ok := release.Snapshot().FindCodeRoute(r.PathValue("slug"))
	if !ok {
		codeWriteError(w, r, codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	if !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, f.now()) {
		codeWriteError(w, r, codemode.Refuse(404, "code_route_unavailable"))
		return
	}
	if f.ledger == nil {
		codeWriteError(w, r, codemode.Refuse(503, "code_admission_unavailable"))
		return
	}
	offers, err := f.ledger.Models(r.Context(), route, authority.ID)
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	fault.WriteJSON(w, struct {
		Object string           `json:"object"`
		Data   []codemode.Model `json:"data"`
	}{Object: "list", Data: codeclient.Catalog(route, offers)})
}

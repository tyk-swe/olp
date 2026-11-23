package codeforward

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeclient"
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
	route, keyID, err := f.codeReadRoute(r, "code_route_unavailable")
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	offers, err := f.ledger.Models(r.Context(), route, keyID)
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	fault.WriteJSON(w, struct {
		Object string           `json:"object"`
		Data   []codemode.Model `json:"data"`
	}{Object: "list", Data: codeclient.Catalog(route, offers)})
}

func (f *Forwarder) codeReadRoute(r *http.Request, missing string) (codemode.Route, string, error) {
	token, failure := ingress.PresentedKey(r)
	if failure != nil {
		return codemode.Route{}, "", codemode.Refuse(failure.Status, "code_authentication_refused")
	}
	authority, failure := f.gate.AuthenticateKey(r, token, "inference")
	if failure != nil {
		return codemode.Route{}, "", codemode.Refuse(failure.Status, "code_authentication_refused")
	}
	if authority.TaskCapability != nil {
		return codemode.Route{}, "", codemode.Refuse(403, "code_authentication_refused")
	}
	release := f.runtime.Release()
	if release == nil || release.Snapshot() == nil {
		return codemode.Route{}, "", codemode.Refuse(503, "code_runtime_unavailable")
	}
	route, ok := release.Snapshot().FindCodeRoute(r.PathValue("slug"))
	if !ok || !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, f.now()) {
		return codemode.Route{}, "", codemode.Refuse(404, missing)
	}
	if f.ledger == nil {
		return codemode.Route{}, "", codemode.Refuse(503, "code_admission_unavailable")
	}
	return route, authority.ID, nil
}

package codeforward

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codewire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

func (f *Forwarder) codeThreadStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !f.gate.Admit(r.Context()) {
		codeWriteError(w, r, codemode.Refuse(503, "code_overloaded"))
		return
	}
	defer f.gate.Release(r.Context())
	route, keyID, err := f.codeReadRoute(r, "code_thread_not_found")
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	identity, err := threadIdentity(r.Header)
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	now := f.now().UTC()
	status, err := f.ledger.ThreadStatus(r.Context(), route, keyID, identity, now)
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	if status.RetryAt != nil {
		fault.SetRetryAfter(w.Header(), status.RetryAt.Sub(now))
	}
	fault.WriteJSON(w, status)
}

func threadIdentity(headers http.Header) (codemode.Identity, error) {
	if codewire.CodexHeaders(headers) {
		return codeConnectionIdentity(codeadapter.IdentityCodex, headers)
	}
	return codeConnectionIdentity(codeadapter.IdentityClient, headers)
}

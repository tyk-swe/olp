package codeforward

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/gateway/codeforward/codexwire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
)

func threadIdentity(headers http.Header) (codemode.Identity, error) {
	has := func(names ...string) bool {
		for _, name := range names {
			if len(headers.Values(name)) != 0 {
				return true
			}
		}
		return false
	}
	codex := has("Thread-Id", "Session-Id", "Session_id", "X-Codex-Parent-Thread-Id", "X-Openai-Subagent")
	session := has("X-Opencode-Session-Id", "X-Opencode-Parent-Session-Id", "X-Claude-Code-Session-Id", "X-Claude-Code-Agent-Id", "X-Claude-Code-Parent-Agent-Id")
	if codex && session {
		return codemode.Identity{}, codemode.Refuse(400, "code_identity_ambiguous")
	}
	identity, err := func() (codemode.Identity, error) {
		if codex {
			return codexwire.ConnectionIdentity(headers)
		}
		return codewire.ClientIdentity(headers)
	}()
	if err != nil {
		return identity, err
	}
	if identity.Conversation == "" || strings.HasPrefix(identity.Conversation, "olp:request/") {
		return identity, codemode.Refuse(400, "code_identity_invalid")
	}
	return identity, nil
}

func (f *Forwarder) codeThreadStatus(w http.ResponseWriter, r *http.Request) {
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
	if !ok || !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, f.now()) {
		codeWriteError(w, r, codemode.Refuse(404, "code_thread_not_found"))
		return
	}
	identity, err := threadIdentity(r.Header)
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	if f.ledger == nil {
		codeWriteError(w, r, codemode.Refuse(503, "code_admission_unavailable"))
		return
	}
	status, err := f.ledger.ThreadStatus(r.Context(), route, authority.ID, identity, f.now().UTC())
	if err != nil {
		codeWriteError(w, r, err)
		return
	}
	if retryAfter := (&codemode.Refusal{RetryAt: status.RetryAt}).RetryAfter(f.now()); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(retryAfter.Seconds()), 10))
	}
	fault.WriteJSON(w, status)
}

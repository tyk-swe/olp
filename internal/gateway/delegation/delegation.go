package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/release"
)

var errNotJSON = errors.New("request body is not application/json")

type creator interface {
	CreateDelegation(context.Context, keyauthority.Authority, keyauthority.Authority, release.DelegationBounds) (release.DelegationGrant, error)
}

type consenter interface {
	ExchangeMemberConsent(context.Context, keyauthority.Authority, string, release.DelegationBounds) (release.DelegationGrant, error)
}

// Dependencies are the collaborators delegation creation serves through.
type Dependencies struct {
	Gate  *ingress.Gate
	Keys  consumerauth.Authenticator
	Begin func(http.ResponseWriter, *http.Request)
}

// Handler serves POST /v1/delegations.
type Handler struct {
	gate  *ingress.Gate
	keys  consumerauth.Authenticator
	begin func(http.ResponseWriter, *http.Request)
}

func New(deps Dependencies) *Handler {
	return &Handler{gate: deps.Gate, keys: deps.Keys, begin: deps.Begin}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.begin != nil {
		h.begin(w, r)
	}
	if !h.gate.Admit(r.Context()) {
		fault.WriteError(w, fault.Overloaded)
		return
	}
	defer h.gate.Release(r.Context())
	delegate, ok := h.keys.(creator)
	if !ok {
		fault.WriteError(w, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
		return
	}
	assertion, ok := consumerauth.Bearer(r.Header.Get("Authorization"))
	if !ok {
		fault.WriteError(w, fault.AuthenticationError("invalid_api_key", "Provide a workload identity."))
		return
	}
	var bounds release.DelegationBounds
	if err := decodeStrict(r, &bounds, 16<<10); err != nil {
		fault.WriteError(w, fault.InvalidRequest("invalid_request", "Provide bounded delegation scopes and resources.", nil))
		return
	}
	agent, failure := h.gate.AuthenticateKey(r, assertion, scope(bounds))
	if failure != nil {
		fault.WriteError(w, failure)
		return
	}
	if consent := strings.TrimSpace(r.Header.Get("X-OLP-Delegation-Consent")); consent != "" {
		if strings.TrimSpace(r.Header.Get("X-OLP-Delegating-Key")) != "" {
			fault.WriteError(w, fault.InvalidRequest("invalid_request", "Provide either a delegating key or member consent.", nil))
			return
		}
		member, ok := h.keys.(consenter)
		if !ok {
			fault.WriteError(w, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
			return
		}
		grant, err := member.ExchangeMemberConsent(r.Context(), agent, consent, bounds)
		writeGrant(w, grant, err)
		return
	}
	key, failure := h.gate.AuthenticateKey(r, strings.TrimSpace(r.Header.Get("X-OLP-Delegating-Key")), "delegate")
	if failure != nil {
		fault.WriteError(w, failure)
		return
	}
	grant, err := delegate.CreateDelegation(r.Context(), agent, key, bounds)
	writeGrant(w, grant, err)
}

func scope(bounds release.DelegationBounds) string {
	if slices.Contains(bounds.Scopes, "tools") {
		return "tools"
	}
	if slices.Contains(bounds.Scopes, "agents") {
		return "agents"
	}
	return "inference"
}

func decodeStrict(r *http.Request, destination any, limit int64) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errNotJSON
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	return oif.DecodeStrict(data, destination, oif.StrictLimits(limit))
}

func writeGrant(w http.ResponseWriter, grant release.DelegationGrant, err error) {
	if errors.Is(err, release.ErrDelegationBounds) {
		fault.WriteError(w, fault.InvalidRequest("invalid_request", "Provide bounded delegation scopes and resources.", nil))
		return
	}
	if errors.Is(err, release.ErrInvalidKey) {
		fault.WriteError(w, fault.AuthenticationError("invalid_api_key", "The member consent is invalid or expired."))
		return
	}
	if errors.Is(err, release.ErrForbidden) {
		fault.WriteError(w, fault.PermissionError("permission_denied", "The principals cannot delegate the requested rights."))
		return
	}
	if err != nil {
		fault.WriteError(w, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(grant)
}

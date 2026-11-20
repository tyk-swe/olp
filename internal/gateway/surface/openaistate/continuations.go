package openaistate

import (
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s surface) recoverContinuation(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.Chat)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	if values := r.Header.Values(kernel.ContinuationHeader); len(values) != 1 || values[0] != interaction.ContinuationV1 {
		s.kernel.Refuse(x, w, fault.InvalidRequest("state_carrier", "Recovery requires the tested chat-anthropic-tools-v1 client contract.", nil))
		return
	}
	if !authority.Policy.AllowProviderState {
		s.kernel.Refuse(x, w, fault.InvalidRequest("policy_conflict", "Encrypted native continuation requires allow_provider_state on this API key.", nil))
		return
	}
	var res *resources.Resource
	var payload []byte
	var err error
	if submission := r.PathValue("submission"); submission != "" {
		res, payload, err = s.kernel.Deps.Resources.FindSubmission(r.Context(), authority.ID, submission)
	} else {
		res, payload, err = s.kernel.Deps.Resources.ReadContract(r.Context(), resources.KindContinuation, authority.ID, r.PathValue("id"))
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.NotFoundError("continuation_unavailable", "No unexpired continuation with this identity is available to this key."))
		return
	}
	route, exists := x.Snapshot().FindRoute(res.RouteSlug)
	if !exists || !authority.Allows("inference", route.Slug, route.ProjectID, s.kernel.Now()) {
		s.kernel.Refuse(x, w, fault.NotFoundError("continuation_unavailable", "The continuation is unavailable to this key."))
		return
	}
	x.Route = &route
	state, err := kernel.DecodeStoredContinuation(payload)
	if err != nil {
		s.kernel.Refuse(x, w, kernel.ContinuationError("continuation_unavailable", "The stored contract is unavailable."))
		return
	}
	if e := s.kernel.AuthorizeContinuation(r.Context(), x, res, state); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if res.State != resources.StateReady || state.Interaction == nil {
		s.kernel.Refuse(x, w, kernel.ContinuationError("continuation_outcome_unknown", "This submission reserved provider work but has no committed ready delivery; fresh inference will not run."))
		return
	}
	body, err := json.Marshal(map[string]any{"version": state.Version, "handle": res.ID, "state": "ready", "assistant": state.Interaction.Assistant, "delivery": state.Delivery})
	if err != nil {
		s.kernel.Refuse(x, w, kernel.ContinuationError("continuation_unavailable", "The stored delivery could not be encoded."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Should-Retry", "false")
	w.WriteHeader(http.StatusOK)
	deliveryErr := s.kernel.WriteContinuationPayload(w, x, append(body, '\n'))
	s.kernel.Finish(x, kernel.ContinuationDeliveryOutcome(deliveryErr), http.StatusOK)
}

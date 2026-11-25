package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func ProviderStateForbidden() *fault.Error {
	return fault.PermissionError("provider_state_forbidden", "This API key does not allow provider state; provider-retained content requires the allow_provider_state policy.")
}

func (s *Server) responsesStateGate(ctx context.Context, x *Call, authority keyauthority.Authority, parsed *wire.Request) *fault.Error {
	var previous string
	if raw := parsed.Field("previous_response_id"); raw != nil {
		_ = json.Unmarshal(raw, &previous)
	}
	var background, store bool
	if raw := parsed.Field("background"); raw != nil {
		_ = json.Unmarshal(raw, &background)
	}
	if raw := parsed.Field("store"); raw != nil {
		_ = json.Unmarshal(raw, &store)
	}
	conversation := parsed.Field("conversation")
	hasConversation := conversation != nil && !bytes.Equal(bytes.TrimSpace(conversation), []byte("null"))
	if route, ok := x.request.release.Snapshot().FindRoute(parsed.Route); ok && route.Fidelity.Strict() {
		if parsed.Field("store") == nil || bytes.Equal(bytes.TrimSpace(parsed.Field("store")), []byte("null")) {
			// Native Responses omission requests provider retention. Strict admission
			// cannot silently inject store:false to avoid the caller's state policy.
			store = true
		}
		if background && !store {
			param := "store"
			return fault.InvalidRequest("state_carrier", "Background generation requires retained provider state.", &param)
		}
		if !authority.Policy.AllowProviderState && (store || previous != "" || hasConversation) {
			param := "store"
			if previous != "" {
				param = "previous_response_id"
			}
			return fault.InvalidRequest("policy_conflict", "The native invocation retains provider state but this API key does not permit it.", &param)
		}
	}
	if previous == "" && !background && !store && !hasConversation {
		return nil
	}
	if !authority.Policy.AllowProviderState {
		if previous != "" {
			param := "previous_response_id"
			return fault.InvalidRequest("unsupported_stateful_reference", "The gateway does not hold prior responses; previous_response_id is not supported.", &param)
		}
		if background {
			param := "background"
			return fault.InvalidRequest("unsupported_parameter", "Background responses require stateful polling, which the gateway does not provide.", &param)
		}
		param := "store"
		return fault.InvalidRequest("unsupported_parameter", "Storing responses at the provider requires the allow_provider_state policy.", &param)
	}
	if s.Deps.Resources == nil {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is not configured on this installation.")
	}
	if route, ok := x.request.release.Snapshot().FindRoute(parsed.Route); ok && route.Fidelity.Strict() && !s.Deps.Resources.Encrypted() {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Strict retained Responses requires encrypted resource authority.")
	}
	x.providerState = true
	if previous == "" {
		return nil
	}
	res, contract, err := s.ReadResponseResource(ctx, authority.ID, previous)
	if errors.Is(err, resources.ErrNotFound) {
		param := "previous_response_id"
		return fault.InvalidRequest("invalid_previous_response_id", "previous_response_id must name a stored response owned by this key.", &param)
	}
	if err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored response could not be read.")
	}
	if res.RouteSlug != parsed.Route {
		param := "previous_response_id"
		return fault.InvalidRequest("invalid_previous_response_id", "previous_response_id must reference a response created under this model.", &param)
	}
	// A retained response whose contract the route no longer promises is
	// refused when prepare resolves this pin, before any provider work.
	if contract != nil {
		if e := s.authorizeResponseContract(ctx, x, authority, res, contract, RetainedNewWork); e != nil {
			return e
		}
		x.generation.responseContract = contract
		x.serving = &contract.Receipt.Serving
		x.servingSlot = res.SlotID
		x.servingBinding = contract.Binding
	}
	x.pin = res
	x.providerState = true
	encoded, err := json.Marshal(res.UpstreamID)
	if err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored response could not be read.")
	}
	next, err := parsed.WithField("previous_response_id", encoded)
	if err != nil {
		return RequestError(err)
	}
	x.generation.parsed = next
	return nil
}

func (s *Server) pinAttempts(ctx context.Context, x *Call) *fault.Error {
	if x.pin == nil {
		return nil
	}
	p, historical, e := s.ResolveResource(ctx, x, x.authority, x.pin, x.family.Operation(), RetainedNewWork)
	if e != nil {
		return e
	}
	if x.generation.continuation != nil && x.generation.continuation.parentState != nil {
		if e := s.authorizeContinuationPin(ctx, x, x.generation.continuation.parent, x.generation.continuation.parentState, p); e != nil {
			return e
		}
	}
	if !p.Provider.Supports(p.Model, x.family.Operation(), x.family.Surface(), x.mode) {
		return fault.ServerError(http.StatusConflict, "provider_resource_credential_unavailable",
			"The provider that owns this object can no longer serve this operation.")
	}
	// Preserve the historical provider and compiled route contract. Resolving
	// only a slot/attempt and then reading the current provider changes defaults,
	// endpoint or profile behind a retained response/continuation handle.
	current := x.request.release.Snapshot()
	if retained, slot, same := current.PinnedCurrent(*historical, p.Provider, p.Target, p.Slot, x.keyID, x.family.Surface(), x.mode); same {
		// The installed release was already validated and compiled at
		// publication. Reusing its immutable target template avoids compiling
		// the same strict contract again for every continuation turn.
		x.historicalSnapshot = retained
		route := retained.Route(historical.Slug)
		x.Route = &route
		x.attempts = []routing.Attempt{p.attempt}
		x.budget = 1
		x.pinnedSlot = &slot
		return nil
	}
	historical.Targets = []routing.Target{p.Target}
	p.Provider.Slots = []routing.Slot{p.Slot}
	retained, err := current.Historical(p.Provider, *historical)
	if err != nil {
		return PinUnavailable()
	}
	x.historicalSnapshot = retained
	route := retained.Route(historical.Slug)
	x.Route = &route
	x.attempts = []routing.Attempt{p.attempt}
	x.budget = 1
	x.pinnedSlot = &p.Slot
	return nil
}

func responseStoreRequested(parsed *wire.Request) bool {
	raw := parsed.Field("store")
	if raw == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	var store bool
	return json.Unmarshal(raw, &store) == nil && store
}

func responseMetadataFields() [5]string {
	return [5]string{"object", "status", "status_details", "created_at", "expires_at"}
}

// previousResponseLocal resolves an upstream previous_response_id echoed by
// a non-strict provider to the caller's local id. It returns "" when the
// gateway holds no mapping, and the caller then withholds the upstream id.
func (s *Server) previousResponseLocal(ctx context.Context, identity resources.NativeIdentity, pin *resources.Resource, upstream string) string {
	if pin != nil && pin.UpstreamID == upstream {
		return pin.ID
	}
	if s.Deps.Resources == nil {
		return ""
	}
	identity.UpstreamID = upstream
	res, err := s.Deps.Resources.GetByUpstream(ctx, identity)
	if err != nil {
		return ""
	}
	return res.ID
}

// MapPreviousResponse maps a non-strict response object's echoed
// previous_response_id so callers only see gateway-owned response ids.
func (s *Server) MapPreviousResponse(ctx context.Context, body []byte, identity resources.NativeIdentity, pin *resources.Resource) ([]byte, error) {
	upstream, ok := upstreamString(body, "previous_response_id")
	if !ok {
		return body, nil
	}
	return rewritePrevious(body, s.previousResponseLocal(ctx, identity, pin, upstream))
}

package geminiinteractions

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

type previous struct {
	resource *resources.Resource
	upstream string
}

func (s surface) create(w http.ResponseWriter, r *http.Request) {
	t, ok := s.begin(w, r, false)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	defer func() {
		s.kernel.Finish(t.x, t.end.out, t.end.status)
		s.kernel.SettleGeneration(r.Context(), t.x, t.end.pin)
	}()
	input, body, e := t.readGeneration()
	if e != nil {
		t.refuse(e)
		return
	}
	route, e := t.admitGeneration(input)
	if e != nil {
		t.refuse(e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	if e := s.kernel.ReserveGeneration(ctx, t.x, t.authority, len(body), route); e != nil {
		t.refuse(e)
		return
	}
	pin, parent, e := t.pinGeneration(ctx, input, route)
	t.end.pin = pin
	if e != nil {
		t.refuse(e)
		return
	}
	response, e := t.sendGeneration(ctx, pin, input, route, parent)
	if response != nil {
		defer response.Body.Close()
	}
	if e != nil {
		t.refuse(e)
		return
	}
	if input.Stream {
		committed, err := t.relay(ctx, pin, response, binding{store: input.Store, parent: parent.resource})
		t.streamed(committed, err, "The Interaction stream ended before its terminal contract.")
		return
	}
	t.answerGeneration(ctx, pin, response, input, parent)
}

func (t turn) readGeneration() (geminilifecycle.InteractionRequest, []byte, *fault.Error) {
	query, err := url.ParseQuery(t.r.URL.RawQuery)
	if err != nil || len(query["last_event_id"]) > 0 || len(query["stream"]) > 0 || len(query["include_input"]) > 0 {
		return geminilifecycle.InteractionRequest{}, nil, fault.InvalidRequest("invalid_request", "Interaction create does not accept retrieval controls.", nil)
	}
	body, e := t.kernel.Gate.ReadBody(t.r)
	if e != nil {
		return geminilifecycle.InteractionRequest{}, nil, e
	}
	limit := int(t.kernel.Config.MaxBodyBytes)
	if limit <= 0 {
		limit = len(body)
	}
	input, err := geminilifecycle.ParseInteractionRequest(body, limit)
	if err != nil || !routeidentity.ValidSlug(input.Model) {
		return geminilifecycle.InteractionRequest{}, nil, fault.InvalidRequest("invalid_interaction_request", "The Gemini Interaction request is malformed.", nil)
	}
	if input.Stream {
		t.x.Streams()
	}
	return input, body, nil
}

func (t turn) admitGeneration(input geminilifecycle.InteractionRequest) (*routing.Route, *fault.Error) {
	found, ok := t.x.Snapshot().FindRoute(input.Model)
	if !ok {
		return nil, fault.ModelNotFound(input.Model)
	}
	route := &found
	t.x.Route = route
	if !t.authority.Allows("inference", route.Slug, route.ProjectID, t.kernel.Now()) {
		return nil, fault.ModelNotFound(input.Model)
	}
	if !slices.Contains(route.Operations, "generation") {
		return nil, fault.InvalidRequest("invalid_request", "This route does not allow Interactions.", nil)
	}
	if e := t.x.GuardrailSurfaceGate(route); e != nil {
		return nil, e
	}
	if e := t.x.CheckBody(route); e != nil {
		return nil, e
	}
	if input.Store || input.Previous != "" || input.Background {
		if !t.authority.Policy.AllowProviderState {
			return nil, fault.InvalidRequest("policy_conflict", "This API key does not allow provider-retained Interaction state.", nil)
		}
		if !t.stateConfigured() {
			return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Encrypted Interaction state is not configured.")
		}
	}
	if e := t.x.RoutingControls(t.r, t.authority); e != nil {
		return nil, e
	}
	if input.Background {
		if e := kernel.UnboundedWorkLimits(t.x.AdmissionAuthority(t.authority), nil, nil, route); e != nil {
			return nil, e
		}
	}
	return route, nil
}

func (t turn) pinGeneration(ctx context.Context, input geminilifecycle.InteractionRequest, route *routing.Route) (*kernel.Pin, previous, *fault.Error) {
	if input.Previous == "" {
		pin, e := t.kernel.SelectGenerationPin(ctx, t.x, route, "gemini", t.x.Mode(), input.MaxOutputTokens, func(provider *routing.Provider, model string) bool {
			return provider.ProfileID == profile && provider.Connector().Supports("generation", "gemini", t.x.Mode()) && provider.Supports(model, "generation", "gemini", t.x.Mode())
		})
		return pin, previous{}, e
	}
	parent, upstream, err := t.kernel.Deps.Resources.ReadInteractionContract(ctx, t.authority.ID, input.Previous)
	if errors.Is(err, resources.ErrNotFound) || parent != nil && parent.RouteSlug != input.Model {
		return nil, previous{}, fault.InvalidRequest("invalid_previous_interaction_id", "The previous Interaction is not owned by this key and route.", new("previous_interaction_id"))
	}
	if err != nil {
		return nil, previous{}, fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The previous Interaction cannot be read.")
	}
	pin, retained, e := t.kernel.ResolveResource(ctx, t.x, t.authority, parent, "generation", kernel.RetainedNewWork)
	if e != nil || retained == nil || retained.Slug != route.Slug || !t.supports(&pin.Provider, pin.Model) {
		return pin, previous{}, kernel.PinUnavailable()
	}
	if e := t.kernel.GatePinnedGeneration(ctx, t.x, pin, "generation", input.MaxOutputTokens); e != nil {
		return pin, previous{}, e
	}
	return pin, previous{resource: parent, upstream: upstream}, nil
}

func (t turn) sendGeneration(ctx context.Context, pin *kernel.Pin, input geminilifecycle.InteractionRequest, route *routing.Route, parent previous) (*kernel.PinnedResponse, *fault.Error) {
	if input.Background {
		if e := kernel.UnboundedWorkLimits(t.x.AdmissionAuthority(t.x.Authority()), &pin.Provider, &pin.Slot, route); e != nil {
			return nil, e
		}
	}
	// Admission ran before the target was known, so the cost hold follows the
	// target's selection, before anything is dispatched.
	if e := t.kernel.ReserveFallbackCost(ctx, t.x); e != nil {
		return nil, e
	}
	connector := pin.Provider.Connector()
	endpoint, err := connector.InteractionsURL("", "")
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Interactions profile cannot address this model.")
	}
	bound, err := input.Bind(connector.Model(pin.Model), parent.upstream)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_interaction_request", "The Interaction identities could not be bound.", nil)
	}
	response, failure := t.kernel.PinnedDo(ctx, t.x, pin, http.MethodPost, endpoint, bound, "application/json")
	if failure != nil {
		return nil, failure.Fault()
	}
	if response.StatusCode != http.StatusOK {
		return response, unsupportedStatus()
	}
	return response, nil
}

func (t turn) answerGeneration(ctx context.Context, pin *kernel.Pin, response *kernel.PinnedResponse, input geminilifecycle.InteractionRequest, parent previous) {
	result, e := t.readResult(response, "The provider returned an invalid Interaction resource.")
	if e != nil {
		t.refuse(e)
		return
	}
	kernel.RecordInteractionUsage(response.Exchange, result.Source.Root())
	switch {
	case result.Status == "failed":
		response.Exchange.Complete(kernel.ClassUpstreamServer)
		t.x.Failure = generationFailure()
	case geminilifecycle.InteractionTurnFinished(result.Status):
		response.Exchange.Complete(kernel.ClassSuccess)
	default:
		response.Exchange.AcceptUnfinished()
	}
	projection, e := t.projectGeneration(ctx, pin, response, input, parent, result)
	if e != nil {
		t.refuse(e)
		return
	}
	t.deliver(response, projection, "The Interaction result could not be delivered.")
}

func (t turn) projectGeneration(ctx context.Context, pin *kernel.Pin, response *kernel.PinnedResponse, input geminilifecycle.InteractionRequest, parent previous, result geminilifecycle.InteractionResponse) ([]byte, *fault.Error) {
	localID := result.ID
	if input.Store {
		unfinished := !geminilifecycle.InteractionTurnFinished(result.Status)
		stored, err := t.kernel.StoreInteraction(ctx, response.Exchange, pin, t.authority.ID, result.ID, result.Status, parent.resource, unfinished)
		if err != nil {
			return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction mapping could not be committed.")
		}
		localID = stored.ID
	}
	safe, e := t.withoutCredentials(result)
	if e != nil {
		return nil, e
	}
	projection, err := safe.WithProjection(localID, input.Previous, input.Model)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound resource reference.")
	}
	if input.Store && containsNativeResourceID(projection, result.ID, parent.upstream) {
		return nil, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound native resource identity.")
	}
	return projection, t.bounded(projection)
}

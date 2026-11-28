package geminiinteractions

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

type controls struct {
	stream  bool
	resumed bool
	forward url.Values
}

type stored struct {
	id       string
	resource *resources.Resource
	upstream string
	route    *routing.Route
}

func (i stored) parentID() string {
	if i.resource.ParentID == nil {
		return ""
	}
	return resources.LocalID(resources.KindInteraction, *i.resource.ParentID)
}

func (s surface) resource(w http.ResponseWriter, r *http.Request) {
	t, ok := s.begin(w, r, true)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	defer func() {
		s.kernel.Finish(t.x, t.end.out, t.end.status)
		s.kernel.SettleRetrieval(r.Context(), t.x, t.end.pin)
	}()
	asked, e := t.retrievalControls()
	if e != nil {
		t.refuse(e)
		return
	}
	interaction, e := t.storedInteraction()
	if e != nil {
		t.refuse(e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), interaction.route)
	defer cancel()
	if e := s.kernel.ReserveRetrieval(ctx, t.x, t.authority, interaction.route); e != nil {
		t.refuse(e)
		return
	}
	pin, e := t.pinStored(ctx, interaction)
	t.end.pin = pin
	if e != nil {
		t.refuse(e)
		return
	}
	endpoint, e := t.storedURL(pin, interaction, asked)
	if e != nil {
		t.refuse(e)
		return
	}
	response, failure := s.kernel.PinnedDo(ctx, t.x, pin, r.Method, endpoint, nil, "")
	if r.Method == http.MethodDelete && (failure == nil || failure.Status == http.StatusNotFound) {
		e := t.forget(ctx, response, interaction)
		if e == nil {
			return
		}
		if failure == nil {
			t.refuse(e)
			return
		}
	}
	if failure != nil {
		t.refuse(failure.Fault())
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.refuse(unsupportedStatus())
		return
	}
	if asked.stream {
		committed, err := t.relay(ctx, pin, response, binding{store: true, existing: interaction.resource, upstream: interaction.upstream, resumed: asked.resumed})
		t.streamed(committed, err, "The Interaction retrieval stream ended before its terminal contract.")
		return
	}
	t.answerStored(ctx, response, interaction)
}

func (t turn) retrievalControls() (controls, *fault.Error) {
	if !t.stateConfigured() || !t.authority.Policy.AllowProviderState {
		return controls{}, fault.PermissionError("provider_state_unavailable", "Interaction state is unavailable to this key.")
	}
	query, err := url.ParseQuery(t.r.URL.RawQuery)
	if err != nil {
		return controls{}, fault.InvalidRequest("invalid_request", "The Interaction query is malformed.", nil)
	}
	if value, found := query["stream"]; found && value[0] != "true" && value[0] != "false" {
		return controls{}, fault.InvalidRequest("invalid_request", "stream must be true or false.", nil)
	}
	stream := query.Get("stream") == "true"
	if stream {
		t.x.Streams()
	}
	resumed := len(query["last_event_id"]) > 0
	if t.r.Method != http.MethodGet && (len(query["stream"]) > 0 || resumed || len(query["include_input"]) > 0) || resumed && !stream {
		return controls{}, fault.InvalidRequest("invalid_request", "Interaction retrieval controls are invalid for this method.", nil)
	}
	if values := query["include_input"]; len(values) > 0 && values[0] != "true" && values[0] != "false" {
		return controls{}, fault.InvalidRequest("invalid_request", "include_input must be true or false.", nil)
	}
	if resumed && len(query["last_event_id"][0]) > 512 {
		return controls{}, fault.InvalidRequest("invalid_request", "last_event_id is too long.", nil)
	}
	forward := url.Values{}
	for _, name := range []string{"stream", "last_event_id", "include_input"} {
		if values := query[name]; len(values) == 1 {
			forward.Set(name, values[0])
		}
	}
	return controls{stream: stream, resumed: resumed, forward: forward}, nil
}

func (t turn) storedInteraction() (stored, *fault.Error) {
	id := t.r.PathValue("id")
	resource, upstream, err := t.store.ReadInteractionContract(t.r.Context(), t.authority.ID, id)
	if errors.Is(err, resources.ErrNotFound) {
		return stored{}, unknownInteraction()
	}
	if err != nil {
		return stored{}, fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The Interaction mapping cannot be read.")
	}
	found, ok := t.x.Snapshot().FindRoute(resource.RouteSlug)
	if !ok || !t.authority.Allows("inference", found.Slug, found.ProjectID, t.kernel.Now()) {
		return stored{}, unknownInteraction()
	}
	route := &found
	t.x.Route = route
	if e := t.x.GuardrailSurfaceGate(route); e != nil {
		return stored{}, e
	}
	return stored{id: id, resource: resource, upstream: upstream, route: route}, nil
}

func (t turn) pinStored(ctx context.Context, interaction stored) (*kernel.Pin, *fault.Error) {
	use := kernel.RetainedHousekeeping
	if t.r.Method == http.MethodGet {
		use = kernel.RetainedRetrieval
	}
	pin, retained, e := t.kernel.ResolveResource(ctx, t.x, t.authority, interaction.resource, "generation", use)
	if e != nil || retained == nil || retained.Slug != interaction.route.Slug || !t.supports(&pin.Provider, pin.Model) {
		return pin, kernel.PinUnavailable()
	}
	return pin, t.kernel.GatePinnedRetrieval(ctx, t.x, pin)
}

func (t turn) storedURL(pin *kernel.Pin, interaction stored, asked controls) (string, *fault.Error) {
	action := ""
	if t.r.Method == http.MethodPost {
		action = "cancel"
	}
	endpoint, err := pin.Provider.Connector().InteractionsURL(interaction.upstream, action)
	if err != nil {
		return "", kernel.PinUnavailable()
	}
	if t.r.Method != http.MethodGet || len(asked.forward) == 0 {
		return endpoint, nil
	}
	return endpoint + "?" + asked.forward.Encode(), nil
}

func (t turn) forget(ctx context.Context, response *kernel.PinnedResponse, interaction stored) *fault.Error {
	if response != nil {
		defer response.Body.Close()
		response.Exchange.Complete(kernel.ClassSuccess)
	}
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	err := t.store.DeleteInteractionContract(commitCtx, t.authority.ID, interaction.id)
	stopCommit()
	if err != nil {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The deleted Interaction could not be tombstoned.")
	}
	if response == nil {
		t.kernel.WriteStateJSONStatus(t.w, t.x, nil, http.StatusOK)
		return nil
	}
	t.kernel.WriteStateJSONStatus(t.w, t.x, nil, http.StatusOK, response.Exchange)
	return nil
}

func (t turn) answerStored(ctx context.Context, response *kernel.PinnedResponse, interaction stored) {
	const invalid = "The provider returned an invalid or different Interaction."
	result, e := t.readResult(response, invalid)
	if e != nil {
		t.refuse(e)
		return
	}
	if result.ID != interaction.upstream {
		t.refuse(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", invalid))
		return
	}
	// A matching native result belongs to the original generation even when
	// its owner-scoped projection cannot be delivered.
	response.Exchange.Complete(kernel.ClassSuccess)
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	err := t.observeInteraction(commitCtx, t.authority.ID, interaction.id, result.Status, result.Source.Root())
	stopCommit()
	if err != nil {
		t.refuse(fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction state could not be updated."))
		return
	}
	projection, e := t.projectStored(interaction, result)
	if e != nil {
		t.refuse(e)
		return
	}
	t.deliver(response, projection, "The Interaction resource could not be delivered.")
}

func (t turn) projectStored(interaction stored, result geminilifecycle.InteractionResponse) ([]byte, *fault.Error) {
	safe, e := t.withoutCredentials(result)
	if e != nil {
		return nil, e
	}
	projection, err := safe.WithProjection(interaction.id, interaction.parentID(), interaction.resource.RouteSlug)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound Interaction reference.")
	}
	if containsNativeResourceID(projection, interaction.upstream) {
		return nil, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound native resource identity.")
	}
	return projection, t.bounded(projection)
}

func unknownInteraction() *fault.Error {
	return fault.NotFoundError("interaction_not_found", "This Interaction does not exist for this key.")
}

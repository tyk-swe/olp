package geminiinteractions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s surface) beginGeminiInteraction(w http.ResponseWriter, r *http.Request, resourceCall bool) (*kernel.Call, keyauthority.Authority, bool) {
	return s.kernel.Enter(w, r, wire.GeminiInteractions, resourceCall, func(r *http.Request) (keyauthority.Authority, *fault.Error) {
		if _, e := kernel.GeminiClientKey(r, "stream", "last_event_id", "include_input"); e != nil {
			return keyauthority.Authority{}, e
		}
		return s.kernel.Gate.Authenticate(r, "inference")
	})
}

func (s surface) geminiInteractionCreate(w http.ResponseWriter, r *http.Request) {
	x, authority, ok := s.beginGeminiInteraction(w, r, false)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	status := http.StatusInternalServerError
	var out *kernel.Outcome
	var p *kernel.Pin
	defer func() {
		s.kernel.Finish(x, out, status)
		s.kernel.SettleGeneration(r.Context(), x, p)
	}()
	fail := func(e *fault.Error) {
		x.Failure, status = e, e.Status
		if x.Dispatched() && x.Strict() && e.Status >= 500 {
			copy := *e
			copy.NoRetry = true
			e = &copy
		}
		fault.WriteSurfaceError(w, e, "gemini")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["last_event_id"]) > 0 || len(query["stream"]) > 0 || len(query["include_input"]) > 0 {
		fail(fault.InvalidRequest("invalid_request", "Interaction create does not accept retrieval controls.", nil))
		return
	}
	body, e := s.kernel.Gate.ReadBody(r)
	if e != nil {
		fail(e)
		return
	}
	maxBody := int(s.kernel.Config.MaxBodyBytes)
	if maxBody <= 0 {
		maxBody = len(body)
	}
	input, err := geminilifecycle.ParseInteractionRequest(body, maxBody)
	if err != nil || !routeidentity.ValidSlug(input.Model) {
		fail(fault.InvalidRequest("invalid_interaction_request", "The Gemini Interaction request is malformed.", nil))
		return
	}
	if input.Stream {
		x.Streams()
	}
	route, found := x.Snapshot().FindRoute(input.Model)
	if !found {
		fail(fault.ModelNotFound(input.Model))
		return
	}
	x.Route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.kernel.Now()) {
		fail(fault.ModelNotFound(input.Model))
		return
	}
	if !slices.Contains(route.Operations, "generation") {
		fail(fault.InvalidRequest("invalid_request", "This route does not allow Interactions.", nil))
		return
	}
	if e := x.GuardrailSurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	if e := x.CheckBody(&route); e != nil {
		fail(e)
		return
	}
	if input.Store || input.Previous != "" || input.Background {
		if !authority.Policy.AllowProviderState {
			fail(fault.InvalidRequest("policy_conflict", "This API key does not allow provider-retained Interaction state.", nil))
			return
		}
		if s.kernel.Deps.Resources == nil || !s.kernel.Deps.Resources.Encrypted() || s.kernel.Deps.Resolver == nil {
			fail(fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Encrypted Interaction state is not configured."))
			return
		}
	}
	if e = x.RoutingControls(r, authority); e != nil {
		fail(e)
		return
	}
	if input.Background {
		if e := kernel.UnboundedWorkLimits(x.AdmissionAuthority(authority), nil, nil, &route); e != nil {
			fail(e)
			return
		}
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), &route)
	defer cancel()
	if e = s.kernel.ReserveGeneration(ctx, x, authority, len(body), &route); e != nil {
		fail(e)
		return
	}
	var parent *resources.Resource
	previousUpstream := ""
	if input.Previous != "" {
		parent, previousUpstream, err = s.kernel.Deps.Resources.ReadInteractionContract(ctx, authority.ID, input.Previous)
		if errors.Is(err, resources.ErrNotFound) || parent != nil && parent.RouteSlug != input.Model {
			fail(fault.InvalidRequest("invalid_previous_interaction_id", "The previous Interaction is not owned by this key and route.", new("previous_interaction_id")))
			return
		}
		if err != nil {
			fail(fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The previous Interaction cannot be read."))
			return
		}
		var retainedRoute *routing.Route
		p, retainedRoute, e = s.kernel.ResolveResource(ctx, x, authority, parent, "generation", kernel.RetainedNewWork)
		if e != nil || retainedRoute == nil || retainedRoute.Slug != route.Slug || p.Provider.ProfileID != "gemini-interactions" || !p.Provider.Supports(p.Model, "generation", "gemini", x.Mode()) {
			fail(kernel.PinUnavailable())
			return
		}
		if e = s.kernel.GatePinnedGeneration(ctx, x, p, "generation"); e != nil {
			fail(e)
			return
		}
	} else {
		p, e = s.kernel.SelectPinSurface(ctx, x, &route, "generation", "gemini", x.Mode(), func(provider *routing.Provider, model string) bool {
			return provider.ProfileID == "gemini-interactions" && provider.Connector().Supports("generation", "gemini", x.Mode()) && provider.Supports(model, "generation", "gemini", x.Mode())
		})
		if e != nil {
			fail(e)
			return
		}
	}
	if input.Background {
		if e := kernel.UnboundedWorkLimits(x.AdmissionAuthority(x.Authority()), &p.Provider, &p.Slot, &route); e != nil {
			fail(e)
			return
		}
	}
	// Admission ran before the target was known, so the cost hold follows the
	// target's selection, before anything is dispatched.
	if e = s.kernel.ReserveFallbackCost(ctx, x); e != nil {
		fail(e)
		return
	}
	endpoint, err := p.Provider.Connector().InteractionsURL("", "")
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_error", "The Interactions profile cannot address this model."))
		return
	}
	bound, err := input.Bind(p.Provider.Connector().Model(p.Model), previousUpstream)
	if err != nil {
		fail(fault.InvalidRequest("invalid_interaction_request", "The Interaction identities could not be bound.", nil))
		return
	}
	response, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodPost, endpoint, bound, "application/json")
	if failure != nil {
		fail(failure.Fault())
		return
	}

	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unsupported Interaction status."))
		return
	}
	if input.Stream {
		var committed bool
		var streamErr error
		committed, streamErr = s.streamGeminiInteraction(ctx, w, x, p, response, authority, input.Store, parent, nil, "", false)
		if streamErr != nil {
			if errors.Is(streamErr, fault.ErrClientWrite) || r.Context().Err() != nil {
				x.Failure = kernel.ClassFailure(kernel.ClassCancelled)
				x.Failure.NoRetry = true
				status, out = 0, &kernel.Outcome{Err: x.Failure, Committed: committed, Cancelled: true}
				return
			}
			x.Failure = fault.ServerError(http.StatusBadGateway, "interaction_incomplete", "The Interaction stream ended before its terminal contract.")
			status = x.Failure.Status
			if !committed {
				fault.WriteSurfaceError(w, x.Failure, "gemini")
			}
			out = &kernel.Outcome{Err: x.Failure, Committed: committed}
			return
		}
		status, out = http.StatusOK, &kernel.Outcome{Err: x.Failure, Committed: true}
		return
	}
	limit := s.kernel.Config.MaxResponseBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	raw, err := response.ReadBody(limit)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Interaction response exceeded its configured limit."))
		return
	}
	result, err := geminilifecycle.ParseInteractionResponse(raw, int(limit))
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid Interaction resource."))
		return
	}
	terminal := geminilifecycle.InteractionTurnFinished(result.Status)
	nativeFailure := result.Status == "failed"
	kernel.RecordInteractionUsage(response.Exchange, result.Source.Root())
	switch {
	case nativeFailure:
		response.Exchange.Complete(kernel.ClassUpstreamServer)
		x.Failure = geminiGenerationFailure()
	case terminal:
		response.Exchange.Complete(kernel.ClassSuccess)
	default:
		response.Exchange.AcceptUnfinished()
	}
	localID := result.ID
	if input.Store {
		stored, storeErr := s.kernel.StoreInteraction(ctx, response.Exchange, p, authority.ID, result.ID, result.Status, parent, !terminal)
		if storeErr != nil {
			fail(fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction mapping could not be committed."))
			return
		}
		localID = stored.ID
	}
	if nativeFailure {
		result.Source, err = fault.RedactNativeFailureDocument(result.Source, x.Sensitive())
		if err != nil {
			fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction failure could not be safely delivered."))
			return
		}
	}
	projection, err := result.WithProjection(localID, input.Previous, input.Model)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound resource reference."))
		return
	}
	if input.Store && containsNativeResourceID(projection, result.ID, previousUpstream) {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction result contains an unbound native resource identity."))
		return
	}
	if int64(len(projection)) > limit {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The projected Interaction exceeded its configured limit."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	x.ResponseHeaders(w.Header(), false)
	w.Header().Set("X-Should-Retry", "false")
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	w.WriteHeader(response.StatusCode)
	n, writeErr := w.Write(projection)
	if writeErr == nil && n == len(projection) {
		writeErr = controller.Flush()
	}
	if writeErr != nil || n != len(projection) {
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Interaction result could not be delivered.")
		x.Failure.NoRetry = true
		status, out = 0, &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: true}
		response.Exchange.Delivery(kernel.DeliveryState(n))
		return
	}
	x.Delivered(s.kernel.Now())
	status, out = response.StatusCode, &kernel.Outcome{Err: x.Failure, Committed: true}
	response.Exchange.Delivery(usage.ClientTerminal)
}

// Untouched OIF strings keep their source escape spelling. Inspect decoded
// values as well as member names before exposing an owner-scoped projection:
// a raw byte search misses IDs such as "int\\u005ffoo".
func containsNativeResourceID(body []byte, ids ...string) bool {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil {
		return true
	}
	contains := func(value string) bool {
		for _, id := range ids {
			if id != "" && strings.Contains(value, id) {
				return true
			}
		}
		return false
	}
	var visit func(oif.Value) bool
	visit = func(value oif.Value) bool {
		switch value.Kind() {
		case oif.String:
			text, ok := value.Text()
			return !ok || contains(text)
		case oif.Object:
			for _, member := range value.Members() {
				if contains(member.Name) || visit(member.Value) {
					return true
				}
			}
		case oif.Array:
			for _, item := range value.Elements() {
				if visit(item) {
					return true
				}
			}
		}
		return false
	}
	return visit(doc.Root())
}

func (s surface) geminiInteractionResource(w http.ResponseWriter, r *http.Request) {
	x, authority, ok := s.beginGeminiInteraction(w, r, true)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	status := http.StatusInternalServerError
	var out *kernel.Outcome
	var p *kernel.Pin
	defer func() {
		s.kernel.Finish(x, out, status)
		s.kernel.SettleRetrieval(r.Context(), x, p)
	}()
	fail := func(e *fault.Error) {
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, "gemini")
	}
	if s.kernel.Deps.Resources == nil || !s.kernel.Deps.Resources.Encrypted() || s.kernel.Deps.Resolver == nil || !authority.Policy.AllowProviderState {
		fail(fault.PermissionError("provider_state_unavailable", "Interaction state is unavailable to this key."))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(fault.InvalidRequest("invalid_request", "The Interaction query is malformed.", nil))
		return
	}
	stream := false
	if value, found := query["stream"]; found {
		if value[0] != "true" && value[0] != "false" {
			fail(fault.InvalidRequest("invalid_request", "stream must be true or false.", nil))
			return
		}
		stream = value[0] == "true"
	}
	if stream {
		x.Streams()
	}
	if r.Method != http.MethodGet && (len(query["stream"]) > 0 || len(query["last_event_id"]) > 0 || len(query["include_input"]) > 0) || len(query["last_event_id"]) > 0 && !stream {
		fail(fault.InvalidRequest("invalid_request", "Interaction retrieval controls are invalid for this method.", nil))
		return
	}
	if values := query["include_input"]; len(values) > 0 && values[0] != "true" && values[0] != "false" {
		fail(fault.InvalidRequest("invalid_request", "include_input must be true or false.", nil))
		return
	}
	if values := query["last_event_id"]; len(values) > 0 && len(values[0]) > 512 {
		fail(fault.InvalidRequest("invalid_request", "last_event_id is too long.", nil))
		return
	}
	localID := r.PathValue("id")
	res, upstreamID, err := s.kernel.Deps.Resources.ReadInteractionContract(r.Context(), authority.ID, localID)
	if errors.Is(err, resources.ErrNotFound) {
		fail(fault.NotFoundError("interaction_not_found", "This Interaction does not exist for this key."))
		return
	}
	if err != nil {
		fail(fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The Interaction mapping cannot be read."))
		return
	}
	current, found := x.Snapshot().FindRoute(res.RouteSlug)
	if !found || !authority.Allows("inference", current.Slug, current.ProjectID, s.kernel.Now()) {
		fail(fault.NotFoundError("interaction_not_found", "This Interaction does not exist for this key."))
		return
	}
	x.Route = &current
	if e := x.GuardrailSurfaceGate(&current); e != nil {
		fail(e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), &current)
	defer cancel()
	if e := s.kernel.ReserveRetrieval(ctx, x, authority, &current); e != nil {
		fail(e)
		return
	}
	var retained *routing.Route
	var e *fault.Error
	use := kernel.RetainedHousekeeping
	if r.Method == http.MethodGet {
		use = kernel.RetainedRetrieval
	}
	p, retained, e = s.kernel.ResolveResource(ctx, x, authority, res, "generation", use)
	if e != nil || retained == nil || retained.Slug != current.Slug || p.Provider.ProfileID != "gemini-interactions" || !p.Provider.Supports(p.Model, "generation", "gemini", x.Mode()) {
		fail(kernel.PinUnavailable())
		return
	}
	if e = s.kernel.GatePinnedRetrieval(ctx, x, p); e != nil {
		fail(e)
		return
	}
	method, action := r.Method, ""
	if r.Method == http.MethodPost {
		if !strings.HasSuffix(r.URL.Path, "/cancel") {
			fail(fault.NotFoundError("not_found", "Unknown Interaction action."))
			return
		}
		action = "cancel"
	}
	endpoint, err := p.Provider.Connector().InteractionsURL(upstreamID, action)
	if err != nil {
		fail(kernel.PinUnavailable())
		return
	}
	if method == http.MethodGet {
		upstreamQuery := url.Values{}
		for _, name := range []string{"stream", "last_event_id", "include_input"} {
			if values := query[name]; len(values) == 1 {
				upstreamQuery.Set(name, values[0])
			}
		}
		if len(upstreamQuery) > 0 {
			endpoint += "?" + upstreamQuery.Encode()
		}
	}
	response, failure := s.kernel.PinnedDo(ctx, x, p, method, endpoint, nil, "")
	if method == http.MethodDelete && (failure == nil || failure.Status == http.StatusNotFound) {
		deletionErr := s.finishGeminiInteractionDeletion(ctx, w, x, response, func(commitCtx context.Context) error {
			return s.kernel.Deps.Resources.DeleteInteractionContract(commitCtx, authority.ID, localID)
		})
		if deletionErr == nil {
			return
		}
		if failure == nil {
			fail(deletionErr)
			return
		}
	}
	if failure != nil {
		fail(failure.Fault())
		return
	}

	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unsupported Interaction status."))
		return
	}
	if stream {
		committed, streamErr := s.streamGeminiInteraction(ctx, w, x, p, response, authority, true, nil, res, upstreamID, len(query["last_event_id"]) > 0)
		if streamErr != nil {
			if errors.Is(streamErr, fault.ErrClientWrite) || r.Context().Err() != nil {
				x.Failure = kernel.ClassFailure(kernel.ClassCancelled)
				status, out = 0, &kernel.Outcome{Err: x.Failure, Committed: committed, Cancelled: true}
				return
			}
			x.Failure = fault.ServerError(http.StatusBadGateway, "interaction_incomplete", "The Interaction retrieval stream ended before its terminal contract.")
			status = x.Failure.Status
			if !committed {
				fault.WriteSurfaceError(w, x.Failure, "gemini")
			}
			out = &kernel.Outcome{Err: x.Failure, Committed: committed}
			return
		}
		status, out = http.StatusOK, &kernel.Outcome{Committed: true}
		return
	}
	limit := s.kernel.Config.MaxResponseBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	raw, err := response.ReadBody(limit)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Interaction response exceeded its configured limit."))
		return
	}
	result, err := geminilifecycle.ParseInteractionResponse(raw, int(limit))
	if err != nil || result.ID != upstreamID {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an invalid or different Interaction."))
		return
	}
	// A matching native result belongs to the original generation even when
	// its owner-scoped projection cannot be delivered.
	response.Exchange.Complete(kernel.ClassSuccess)
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	_, err = s.kernel.ObserveInteraction(commitCtx, authority.ID, localID, result.Status, result.Source.Root())
	stopCommit()
	if err != nil {
		fail(fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Interaction state could not be updated."))
		return
	}
	parentID := ""
	if res.ParentID != nil {
		parentID = resources.LocalID(resources.KindInteraction, *res.ParentID)
	}
	if result.Status == "failed" {
		result.Source, err = fault.RedactNativeFailureDocument(result.Source, x.Sensitive())
		if err != nil {
			fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction failure could not be safely delivered."))
			return
		}
	}
	projection, err := result.WithProjection(localID, parentID, res.RouteSlug)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound Interaction reference."))
		return
	}
	if containsNativeResourceID(projection, upstreamID) {
		fail(fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unbound native resource identity."))
		return
	}
	if int64(len(projection)) > limit {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The projected Interaction exceeded its configured limit."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	x.ResponseHeaders(w.Header(), false)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	w.WriteHeader(response.StatusCode)
	n, writeErr := w.Write(projection)
	if writeErr == nil && n == len(projection) {
		writeErr = controller.Flush()
	}
	if writeErr != nil || n != len(projection) {
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Interaction resource could not be delivered.")
		status, out = 0, &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: true}
		response.Exchange.Delivery(kernel.DeliveryState(n))
		return
	}
	x.Delivered(s.kernel.Now())
	response.Exchange.Delivery(usage.ClientTerminal)
	status, out = response.StatusCode, &kernel.Outcome{Committed: true}
}

func (s surface) finishGeminiInteractionDeletion(ctx context.Context, w http.ResponseWriter, x *kernel.Call, response *kernel.PinnedResponse, tombstone func(context.Context) error) *fault.Error {
	if response != nil {
		defer response.Body.Close()
		response.Exchange.Complete(kernel.ClassSuccess)
	}
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	err := tombstone(commitCtx)
	stopCommit()
	if err != nil {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The deleted Interaction could not be tombstoned.")
	}
	if response == nil {
		s.kernel.WriteStateJSONStatus(w, x, nil, http.StatusOK)
		return nil
	}
	s.kernel.WriteStateJSONStatus(w, x, nil, http.StatusOK, response.Exchange)
	return nil
}

func (s surface) streamGeminiInteraction(ctx context.Context, w http.ResponseWriter, x *kernel.Call, p *kernel.Pin, response *kernel.PinnedResponse, authority keyauthority.Authority, store bool, parent, existing *resources.Resource, expectedID string, resumed bool) (bool, error) {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return false, errors.New("provider did not return Interaction SSE")
	}
	limit := int(s.kernel.Config.MaxEventBytes)
	if limit <= 0 {
		limit = 1 << 20
	}
	state := &geminilifecycle.InteractionStream{ID: expectedID, Created: resumed, Resumed: resumed}
	local := existing
	localID := expectedID
	if existing != nil {
		localID = existing.ID
	}
	previous := ""
	if parent != nil {
		previous = parent.ID
	} else if existing != nil && existing.ParentID != nil {
		previous = resources.LocalID(resources.KindInteraction, *existing.ParentID)
	}
	committed := false
	exchange := response.Exchange
	err = sse.Decode(response.Body, limit, func(frame sse.Frame) error {
		event, err := geminilifecycle.ParseInteractionEvent([]byte(frame.Data), limit)
		if err != nil {
			exchange.InvalidNative()
			return err
		}
		if err := state.Accept(event); err != nil {
			exchange.InvalidNative()
			return err
		}
		if expectedID != "" && state.ID != expectedID {
			exchange.InvalidNative()
			return errors.New("provider changed Interaction resource identity")
		}
		if !store && local == nil {
			localID = state.ID
		}
		if object, ok := event.Source.Root().Lookup("interaction"); ok {
			kernel.RecordInteractionUsage(exchange, object)
		}
		if state.Done {
			if !x.ResourceCall() && (event.Type == "error" || event.Status == "failed") {
				exchange.Complete(kernel.ClassUpstreamServer)
				x.Failure = geminiGenerationFailure()
			} else {
				exchange.Complete(kernel.ClassSuccess)
			}
		}
		if store && local == nil {
			initial := event.Status
			if initial == "" {
				initial = "created"
			}
			local, err = s.kernel.StoreInteraction(ctx, response.Exchange, p, authority.ID, state.ID, initial, parent, !state.Done)
			if err != nil {
				return &localRefusal{cause: err}
			}
			localID = local.ID
		}
		observedStatus := event.Status
		if event.Type == "error" {
			observedStatus = "failed"
		}
		if local != nil && observedStatus != "" {
			object, _ := event.Source.Root().Lookup("interaction")
			commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
			_, err := s.kernel.ObserveInteraction(commitCtx, authority.ID, local.ID, observedStatus, object)
			stopCommit()
			if err != nil {
				return &localRefusal{cause: err}
			}
		}
		if event.Type == "error" || event.Status == "failed" {
			event.Source, err = fault.RedactNativeFailureDocument(event.Source, x.Sensitive())
			if err != nil {
				return err
			}
		}
		projected, err := event.WithProjection(localID, previous, x.Route.Slug)
		if err != nil {
			return err
		}
		if local != nil && containsNativeResourceID(projected, state.ID) {
			return errors.New("Interaction event contains an unbound native resource identity")
		}
		var output bytes.Buffer
		if frame.ID != nil {
			output.WriteString("id: ")
			output.WriteString(*frame.ID)
			output.WriteByte('\n')
		}
		if frame.Event != nil {
			output.WriteString("event: ")
			output.WriteString(*frame.Event)
			output.WriteByte('\n')
		}
		if frame.RetryMS != nil {
			output.WriteString("retry: ")
			output.WriteString(strconv.FormatUint(*frame.RetryMS, 10))
			output.WriteByte('\n')
		}
		for line := range bytes.SplitSeq(projected, []byte{'\n'}) {
			output.WriteString("data: ")
			output.Write(line)
			output.WriteByte('\n')
		}
		output.WriteByte('\n')
		if output.Len() > limit {
			return errors.New("projected Interaction event exceeds byte limit")
		}
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
		if !committed {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			x.ResponseHeaders(w.Header(), true)
			if !x.ResourceCall() {
				w.Header().Set("X-Should-Retry", "false")
			}
			w.WriteHeader(http.StatusOK)
			committed = true
			exchange.Delivery(usage.ClientUnobserved)
		}
		if n, err := w.Write(output.Bytes()); err != nil || n != output.Len() {
			if n > 0 {
				exchange.Delivery(usage.ClientPartial)
			}
			return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
		}
		if err := controller.Flush(); err != nil {
			return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
		}
		x.Delivered(s.kernel.Now())
		switch {
		case state.Done:
			exchange.Delivery(usage.ClientTerminal)
		case event.Type == "step.stop":
			exchange.Delivery(usage.ClientActionable)
		default:
			exchange.Delivery(usage.ClientPartial)
		}
		return nil
	})
	if err != nil || !state.Done {
		_, local := errors.AsType[*localRefusal](err)
		switch {
		case errors.Is(err, fault.ErrClientWrite), errors.Is(ctx.Err(), context.Canceled):
			exchange.Interrupt(kernel.ClassCancelled, true)
		case errors.Is(ctx.Err(), context.DeadlineExceeded) && !local:
			exchange.Interrupt(kernel.ClassTimeout, false)
		default:
			exchange.Interrupt(kernel.ClassProtocol, local)
		}
		if err == nil {
			err = errors.New("Interaction SSE ended before terminal event")
		}
		return committed, err
	}
	return committed, nil
}

func geminiGenerationFailure() *fault.Error {
	failure := fault.ServerError(http.StatusBadGateway, "upstream_response_failed", "The provider reported a failed Interaction generation.")
	failure.NoRetry = true
	return failure
}

type localRefusal struct{ cause error }

func (e *localRefusal) Error() string { return "interaction projection or persistence failed" }

func (e *localRefusal) Unwrap() error { return e.cause }

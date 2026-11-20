package retrieval

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/internal/routing"
)

// query uses the ordinary priced admission and pinned exchange. The binding
// is loaded under current authority on every query; it does not grant
// callers discovery of backend resources or permission to mutate an index.
func (s surface) query(w http.ResponseWriter, r *http.Request) {
	t, ok := s.begin(w, r)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	defer func() {
		s.kernel.Finish(t.x, nil, t.end.status)
		s.kernel.SettleRetained(r.Context(), t.x, t.end.pin)
	}()
	binding, e := t.binding()
	if e != nil {
		t.refuse(e)
		return
	}
	route, e := t.route(binding)
	if e != nil {
		t.refuse(e)
		return
	}
	encoded, route, e := t.encoded(binding, route)
	t.x.Route = route
	if e != nil {
		t.refuse(e)
		return
	}
	pin, e := t.pin(binding, route, encoded)
	if e != nil {
		t.refuse(e)
		return
	}
	t.answer(binding, route, encoded, pin)
}

func (t turn) binding() (*retrieval.Binding, *fault.Error) {
	if !t.authority.Policy.AllowProviderState {
		return nil, kernel.ProviderStateForbidden()
	}
	id, err := uuid.Parse(t.r.PathValue("binding_id"))
	if err != nil {
		return nil, fault.NotFoundError("not_found", "No retrieval binding exists for this key.")
	}
	if len(t.r.URL.Query()) > 0 {
		return nil, fault.InvalidRequest("invalid_parameter", "Retrieval does not accept query parameters.", nil)
	}
	if t.kernel.Deps.Resources == nil {
		return nil, fault.ServerError(503, "provider_state_unavailable", "Retrieval bindings are unavailable.")
	}
	tx, readErr := t.kernel.Deps.Resources.Begin(t.r.Context())
	if readErr != nil {
		return nil, fault.ServerError(503, "provider_state_unavailable", "Retrieval bindings are unavailable.")
	}
	binding, readErr := retrieval.Load(t.r.Context(), tx, id.String(), false)
	_ = tx.Rollback(t.r.Context())
	if readErr != nil || binding.RetiredAt != nil || !binding.Enabled || !slices.Contains(binding.KeyGrants, t.authority.ID) || t.authority.ProjectID == nil || *t.authority.ProjectID != binding.ProjectID {
		return nil, fault.NotFoundError("not_found", "No retrieval binding exists for this key.")
	}
	return binding, nil
}

func (t turn) route(binding *retrieval.Binding) (*routing.Route, *fault.Error) {
	candidate, ok := t.x.Snapshot().FindRoute(binding.Route)
	if !ok || candidate.ProjectID == nil || *candidate.ProjectID != binding.ProjectID {
		return nil, fault.ModelNotFound(binding.Route)
	}
	route := candidate
	t.x.Route = &route
	if !t.authority.Allows("inference", route.Slug, route.ProjectID, t.kernel.Now()) {
		return nil, fault.PermissionError("route_forbidden", "The API key cannot use this route.")
	}
	if !slices.Contains(route.Operations, "retrieval") {
		return nil, kernel.PolicyUnavailable("operation_unavailable", "The route does not allow retrieval.")
	}
	if route.Fidelity.Mode == routing.FidelityStrict {
		return nil, kernel.PolicyUnavailable("unsupported_transformation", "Cloud retrieval requires a transformed route.")
	}
	if e := t.x.GuardrailSurfaceGate(&route); e != nil {
		return nil, e
	}
	return &route, nil
}

func (t turn) encoded(binding *retrieval.Binding, route *routing.Route) ([]byte, *routing.Route, *fault.Error) {
	body, e := t.kernel.Gate.ReadBody(t.r)
	if e != nil {
		return nil, route, e
	}
	doc, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: int(t.kernel.Config.MaxBodyBytes)})
	if parseErr != nil || doc.Root().Kind() != oif.Object {
		return nil, route, fault.InvalidRequest("invalid_request", "A valid unambiguous JSON query is required.", nil)
	}
	encoded, encodeErr := retrieval.Encode(binding.Backend, binding.Resource, binding.Fields, body)
	if encodeErr != nil {
		return nil, route, fault.InvalidRequest("unsupported_retrieval_control", encodeErr.Error(), nil)
	}
	// Restrict selection to the binding's one target without changing the
	// serving route revision or widening its certified capability tuple.
	restricted := *route
	restricted.Targets = slices.DeleteFunc(slices.Clone(route.Targets), func(target routing.Target) bool { return target.ID != binding.TargetID })
	if len(restricted.Targets) != 1 || restricted.Targets[0].ProviderModel != binding.Resource {
		return nil, &restricted, kernel.PolicyUnavailable("retrieval_binding_unavailable", "The binding's certified target is unavailable.")
	}
	return encoded, &restricted, nil
}

func (t turn) pin(binding *retrieval.Binding, route *routing.Route, encoded []byte) (*kernel.Pin, *fault.Error) {
	pin, e := t.kernel.SelectRetrievalPin(t.r.Context(), t.x, route, int64(len(encoded)), func(provider *routing.Provider, model string) bool {
		return provider.ID == binding.ProviderID && provider.CredentialSource != "caller" && provider.Connector().RetrievalBackend() == binding.Backend && provider.Connector().Model(model) == binding.Resource && provider.Connector().Supports("retrieval", "openai", "unary")
	})
	t.end.pin = pin
	return pin, e
}

func (t turn) answer(binding *retrieval.Binding, route *routing.Route, encoded []byte, pin *kernel.Pin) {
	endpoint, urlErr := pin.Provider.Connector().RetrievalURL(binding.Resource)
	if urlErr != nil {
		t.refuse(kernel.PolicyUnavailable("retrieval_binding_unavailable", "The backend binding is unavailable."))
		return
	}
	ctx, cancel := t.kernel.StateDeadline(t.r.Context(), route)
	defer cancel()
	if e := t.kernel.AdmitRetainedRequest(ctx, t.x, t.authority, pin, route, false); e != nil {
		t.refuse(e)
		return
	}
	native, failure := t.kernel.PinnedDo(ctx, t.x, pin, http.MethodPost, endpoint, encoded, "application/json")
	if failure != nil {
		t.refuse(kernel.RetainedPublicError(kernel.UpstreamError(failure), failure.Status))
		return
	}
	defer native.Body.Close()
	projected, e := t.result(binding, native)
	if e != nil {
		t.refuse(e)
		return
	}
	t.end.status = http.StatusOK
	t.kernel.WriteStateJSONStatus(t.w, t.x, projected, t.end.status, native.Exchange)
}

func (t turn) result(binding *retrieval.Binding, native *kernel.PinnedResponse) ([]byte, *fault.Error) {
	response, responseErr := native.ReadBody(t.kernel.RetainedResponseLimit())
	if responseErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported retrieval response.")
	}
	results, decodeErr := retrieval.Decode(binding.Backend, binding.Fields, response)
	if decodeErr != nil {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider returned invalid retrieval results.")
	}
	native.Exchange.CompleteRetainedControl()
	projected, marshalErr := json.Marshal(map[string]any{"object": "list", "binding_id": binding.ID, "binding_revision_id": binding.RevisionID, "data": results})
	if marshalErr != nil {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider returned invalid retrieval results.")
	}
	return projected, nil
}

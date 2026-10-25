package gateway

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) registerRetrieval(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/retrieval/{binding_id}/query", s.queryRetrieval)
}

// queryRetrieval uses the ordinary priced admission and pinned exchange. The
// binding is loaded under current authority on every query; it does not grant
// callers discovery of backend resources or permission to mutate an index.
func (s *Server) queryRetrieval(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.release(r.Context())
	x.operationOverride = "retrieval"
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retainedMetered = true
	var p *pin
	status := http.StatusInternalServerError
	defer func() { s.finish(x, nil, status); s.retainedSettle(r.Context(), x, p) }()
	fail := func(err *Error) { x.failure = err; status = err.Status; writeSurfaceError(w, err, "openai") }
	if !authority.Policy.AllowProviderState {
		fail(providerStateForbidden())
		return
	}
	id, err := uuid.Parse(r.PathValue("binding_id"))
	if err != nil {
		fail(notFoundError("not_found", "No retrieval binding exists for this key."))
		return
	}
	if len(r.URL.Query()) > 0 {
		fail(invalidRequest("invalid_parameter", "Retrieval does not accept query parameters.", nil))
		return
	}
	if s.Resources == nil {
		fail(serverError(503, "provider_state_unavailable", "Retrieval bindings are unavailable."))
		return
	}
	tx, readErr := s.Resources.Begin(r.Context())
	if readErr != nil {
		fail(serverError(503, "provider_state_unavailable", "Retrieval bindings are unavailable."))
		return
	}
	binding, readErr := retrieval.Load(r.Context(), tx, id.String(), false)
	_ = tx.Rollback(r.Context())
	if readErr != nil || binding.RetiredAt != nil || !binding.Enabled || !slices.Contains(binding.KeyGrants, x.keyID) || authority.ProjectID == nil || *authority.ProjectID != binding.ProjectID {
		fail(notFoundError("not_found", "No retrieval binding exists for this key."))
		return
	}
	candidate, ok := x.snapshot().Routes[binding.Route]
	if !ok || candidate.ProjectID == nil || *candidate.ProjectID != binding.ProjectID {
		fail(modelNotFound(binding.Route))
		return
	}
	route := candidate
	x.route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		fail(permissionError("route_forbidden", "The API key cannot use this route."))
		return
	}
	if !slices.Contains(route.Operations, "retrieval") {
		fail(policyUnavailable("operation_unavailable", "The route does not allow retrieval."))
		return
	}
	if route.Fidelity.Mode == routing.FidelityStrict {
		fail(policyUnavailable("unsupported_transformation", "Cloud retrieval requires a transformed route."))
		return
	}
	if gateErr := x.guardrailSurfaceGate(&route); gateErr != nil {
		fail(gateErr)
		return
	}
	body, bodyErr := s.readBody(r)
	if bodyErr != nil {
		fail(bodyErr)
		return
	}
	doc, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
	if parseErr != nil || doc.Root().Kind() != oif.Object {
		fail(invalidRequest("invalid_request", "A valid unambiguous JSON query is required.", nil))
		return
	}
	encoded, encodeErr := retrieval.Encode(binding.Backend, binding.Resource, binding.Fields, body)
	if encodeErr != nil {
		fail(invalidRequest("unsupported_retrieval_control", encodeErr.Error(), nil))
		return
	}
	// Restrict selection to the binding's one target without changing the
	// serving route revision or widening its certified capability tuple.
	route.Targets = slices.DeleteFunc(slices.Clone(route.Targets), func(t routing.Target) bool { return t.ID != binding.TargetID })
	if len(route.Targets) != 1 || route.Targets[0].ProviderModel != binding.Resource {
		fail(policyUnavailable("retrieval_binding_unavailable", "The binding's certified target is unavailable."))
		return
	}
	x.estimate = int64(len(encoded))
	p, pinErr := s.selectBoundPinSurface(r.Context(), x, &route, "retrieval", "openai", "unary", nil, func(provider *routing.Provider, model string) bool {
		return provider.ID == binding.ProviderID && provider.CredentialSource != "caller" && provider.Connector().RetrievalBackend() == binding.Backend && provider.Connector().Model(model) == binding.Resource && provider.Connector().Supports("retrieval", "openai", "unary")
	})
	if pinErr != nil {
		fail(pinErr)
		return
	}
	endpoint, urlErr := p.provider.Connector().RetrievalURL(binding.Resource)
	if urlErr != nil {
		fail(policyUnavailable("retrieval_binding_unavailable", "The backend binding is unavailable."))
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	if admissionErr := s.admitRetainedRequest(ctx, x, authority, p, &route, false); admissionErr != nil {
		fail(admissionErr)
		return
	}
	native, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, encoded, "application/json")
	if failure != nil {
		fail(retainedPublicError(upstreamError(failure), failure.status))
		return
	}
	defer native.Body.Close()
	projected, processingErr := s.retrievalResult(x, binding, native)
	if processingErr != nil {
		fail(processingErr)
		return
	}
	status = http.StatusOK
	x.completePinned()
	s.writeStateJSONStatus(w, x, projected, status)
}

func (s *Server) retrievalResult(x *call, binding *retrieval.Binding, native *http.Response) ([]byte, *Error) {
	response, responseErr := readBounded(native.Body, s.retainedResponseLimit())
	if responseErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		return nil, serverError(502, "provider_protocol_error", "The provider returned an unsupported retrieval response.")
	}
	results, decodeErr := retrieval.Decode(binding.Backend, binding.Fields, response)
	if decodeErr != nil {
		return nil, serverError(502, "provider_protocol_error", "The provider returned invalid retrieval results.")
	}
	projected, marshalErr := json.Marshal(map[string]any{"object": "list", "binding_id": binding.ID, "binding_revision_id": binding.RevisionID, "data": results})
	if marshalErr != nil {
		return nil, serverError(502, "provider_protocol_error", "The provider returned invalid retrieval results.")
	}
	// Only a validated result establishes complete tokenless usage. Local
	// processing failures retain the accepted query's billing uncertainty.
	_ = observeRetainedUsage(x, oif.Document{}, false)
	return projected, nil
}

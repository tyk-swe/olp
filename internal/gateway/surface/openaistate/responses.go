package openaistate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/generationrefs"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) responseCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.Responses)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	if !authority.Policy.AllowProviderState {
		s.kernel.Refuse(x, w, kernel.ProviderStateForbidden())
		return
	}
	res, contract, err := s.kernel.ReadResponseResource(r.Context(), authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No stored response with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored response could not be read."))
		return
	}
	p, route, e := s.kernel.AdmitStoredResponse(r.Context(), x, authority, res, contract, use)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.BindResponseContract(contract)
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
	}
}

func (s surface) responseUpstream(ctx context.Context, x *kernel.Call, res *resources.Resource, p *kernel.Pin, method, suffix string, body []byte, w http.ResponseWriter, query url.Values) *fault.Error {
	endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, kernel.ResponsePath(p.Provider.Connector(), suffix), query)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.kernel.PinnedDo(ctx, x, p, method, endpoint, body, "application/json")
	if failure != nil {
		return s.kernel.StoredResponseUpstreamError(ctx, res.ID, failure)
	}
	defer resp.Body.Close()

	result, err := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	doc, parseErr := oif.ParseJSON(result, oif.Limits{MaxBytes: int(s.kernel.Config.MaxResponseBytes)})
	if parseErr != nil {
		if res.Kind == resources.KindStrictResponse {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The retained provider returned an invalid native response.")
		}
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	var native kernel.NativeResponse
	if res.Kind == resources.KindStrictResponse {
		native, err = kernel.ValidateNativeResponse(doc, "", res.UpstreamID)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The retained provider returned an invalid native response identity or grammar.")
		}
	} else {
		id, present := doc.Root().Lookup("id")
		upstream, valid := id.Text()
		if !present || !valid || upstream != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an unbound response identity.")
		}
	}
	observation, err := openai.ObserveResponseDocumentUsage(doc)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned malformed response usage.")
	}
	if observation.Status == "completed" {
		output, present := doc.Root().Lookup("output")
		if !present || output.Kind() != oif.Array {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid completed response output.")
		}
	}
	resp.Exchange.Complete(kernel.ClassSuccess)
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	defer stopCommit()
	if e := s.kernel.ApplyResponseObservation(commitCtx, res.ID, observation); e != nil {
		return e
	}
	nativeStatus := observation.Status
	var out []byte
	if res.Kind == resources.KindStrictResponse {
		projection := kernel.ResponseProjection{UpstreamID: res.UpstreamID, LocalID: res.ID, Route: res.RouteSlug}
		projection.PreviousUpstream, projection.PreviousLocal, err = kernel.ResponseParentProjection(res, x.ResponseContract())
		if err != nil {
			return fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The retained parent response cannot be reconstructed.")
		}
		strictResultDoc, err := native.Project(projection)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The retained provider response could not be projected to its local identities.")
		}
		if nativeStatus == "failed" {
			strictResultDoc, err = fault.RedactNativeFailureDocument(strictResultDoc, x.Sensitive())
			if err != nil {
				return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider failure contained unsafe native fields.")
			}
		}
		out = strictResultDoc.Bytes()
	} else {
		var projected oif.Document
		projected, err = oif.Apply(doc, []oif.Change{
			{Pointer: "/id", Value: kernel.QuotedResponseID(res.ID), Origin: oif.ResourceBinding, Reason: "owner-scoped retained response"},
			{Pointer: "/model", Value: kernel.QuotedResponseID(res.RouteSlug), Origin: oif.IdentityBinding, Reason: "published response model"},
		})
		if err == nil {
			out, err = s.kernel.MapPreviousResponse(commitCtx, projected.Bytes(), res.NativeIdentity(), nil)
		}
	}
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	out, err = s.kernel.MapStoredGenerationResultReferences(commitCtx, x, res, out)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "resource_affinity", "The retained provider returned an unowned result reference.")
	}
	if method == http.MethodPost && res.Kind == resources.KindStrictResponse {
		current, present := x.Snapshot().FindRoute(res.RouteSlug)
		if !present || kernel.RetainedContract(true, current.Fidelity, kernel.RetainedRetrieval) != nil || s.kernel.CheckRouteFidelity(ctx, &current) != nil {
			// Cancellation remains available for cleanup, but cannot return content
			// that the same resource's retrieval guard forbids.
			out, _ = json.Marshal(map[string]string{"id": res.ID, "object": "response", "status": nativeStatus})
		}
	}
	s.kernel.WriteStateJSON(w, x, out, resp.Exchange)
	return nil
}

func (s surface) getResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		query, stream, e := responseRetrievalQuery(r.URL.RawQuery)
		if e != nil {
			return e
		}
		if stream {
			return s.kernel.StreamStoredResponse(ctx, w, x, res, p, query)
		}
		return s.responseUpstream(ctx, x, res, p, http.MethodGet, "/"+url.PathEscape(res.UpstreamID), nil, w, query)
	})
}

func (s surface) cancelResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		return s.responseUpstream(ctx, x, res, p, http.MethodPost, "/"+url.PathEscape(res.UpstreamID)+"/cancel", []byte(`{}`), w, nil)
	})
}

func (s surface) deleteResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, kernel.ResponsePath(p.Provider.Connector(), "/"+url.PathEscape(res.UpstreamID)), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
				defer stopCommit()
				_ = s.kernel.Deps.Resources.Tombstone(commitCtx, res.ID)
				return fault.NotFoundError("not_found", "No stored response with this identifier exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()

		result, _ := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
		resp.Exchange.Complete(kernel.ClassSuccess)
		// The provider already deleted the response: the mapping must follow
		// even when the client has gone.
		commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
		defer stopCommit()
		if err := s.kernel.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The response mapping could not be deleted.")
		}
		out, err := kernel.RewriteID(result, "id", res.ID)
		if err != nil {
			encoded, _ := json.Marshal(res.ID)
			out, _ = json.Marshal(map[string]json.RawMessage{
				"id": encoded, "object": json.RawMessage(`"response"`), "deleted": json.RawMessage(`true`),
			})
		}
		s.kernel.WriteStateJSON(w, x, out, resp.Exchange)
		return nil
	})
}

func (s surface) responseInputItems(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, p *kernel.Pin, route *routing.Route) *fault.Error {
		query := url.Values{}
		for _, name := range []string{"limit", "after", "order"} {
			if value := r.URL.Query().Get(name); value != "" {
				query.Set(name, value)
			}
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, kernel.ResponsePath(p.Provider.Connector(), "/"+url.PathEscape(res.UpstreamID)+"/input_items"), query)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return s.kernel.StoredResponseUpstreamError(ctx, res.ID, failure)
		}
		defer resp.Body.Close()

		result, err := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		document, err := oif.ParseJSON(result, oif.Limits{MaxBytes: int(s.kernel.Config.MaxResponseBytes)})
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned invalid input items.")
		}
		refs, err := generationrefs.InInputItems(document)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned input items with unbound resource references.")
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		projected, err := generationrefs.ProjectInputItems(document, refs, s.kernel.StoredGenerationResultResolver(ctx, x, res))
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned input items with unbound resource references.")
		}
		s.kernel.WriteStateJSON(w, x, projected.Bytes(), resp.Exchange)
		return nil
	})
}

func responseRetrievalQuery(raw string) (url.Values, bool, *fault.Error) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, false, fault.InvalidRequest("invalid_request", "The response retrieval query is malformed.", nil)
	}
	stream := false
	includes := []string{
		"file_search_call.results", "web_search_call.results", "web_search_call.action.sources",
		"message.input_image.image_url", "computer_call_output.output.image_url",
		"code_interpreter_call.outputs", "reasoning.encrypted_content", "message.output_text.logprobs",
	}
	for name, values := range query {
		if len(values) == 0 || len(values) > 16 || name != "include" && name != "include[]" && len(values) != 1 {
			return nil, false, fault.InvalidRequest("invalid_request", "Response retrieval query controls must occur once.", nil)
		}
		switch name {
		case "stream":
			if values[0] != "true" && values[0] != "false" {
				return nil, false, fault.InvalidRequest("invalid_request", "stream must be true or false.", nil)
			}
			stream = values[0] == "true"
		case "starting_after":
			if _, err := strconv.ParseUint(values[0], 10, 63); err != nil {
				return nil, false, fault.InvalidRequest("invalid_request", "starting_after must be a non-negative event sequence.", nil)
			}
		case "include_obfuscation":
			if values[0] != "true" && values[0] != "false" {
				return nil, false, fault.InvalidRequest("invalid_request", "include_obfuscation must be true or false.", nil)
			}
		case "include", "include[]":
			for _, value := range values {
				if !slices.Contains(includes, value) {
					return nil, false, fault.InvalidRequest("invalid_request", "The response include selector is not qualified.", nil)
				}
			}
		default:
			return nil, false, fault.InvalidRequest("invalid_request", "The response retrieval query has an unsupported control.", nil)
		}
	}
	if query.Has("include") && query.Has("include[]") {
		return nil, false, fault.InvalidRequest("invalid_request", "Use one include selector spelling for response retrieval.", nil)
	}
	if !stream && query.Has("starting_after") {
		return nil, false, fault.InvalidRequest("invalid_request", "starting_after requires stream=true.", nil)
	}
	if !stream {
		return query, false, nil
	}
	query.Set("stream", "true")
	return query, true, nil
}
